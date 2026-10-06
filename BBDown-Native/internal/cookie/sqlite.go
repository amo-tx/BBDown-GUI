// Package cookie 从本机浏览器里读出 B 站 Cookie，并做解密。
//
// 为什么要自己解析 SQLite：Chrome / Edge 把 Cookie 存在一个 SQLite 库里，
// 而本项目「零第三方依赖」是硬约束（引一个 SQLite 驱动要带 CGO 或几万行代码）。
// 这里只需要「按表名把整张表读出来」这一个能力，所以手写一个只读的
// B-tree 遍历器是划算的：约 400 行，无 unsafe、无外部 DLL、可单测。
//
// 覆盖到的 SQLite 特性：
//   - 文件头解析（页大小、保留区、文本编码）
//   - 表 B-tree 的叶子页与内部页
//   - 溢出页链（单条 cookie 超过一页时）
//   - 记录格式的 serial type 解码
//   - WAL：把已提交的帧重放到内存镜像上（不重放会读到陈旧的登录状态）
//
// 不支持的（也用不到）：索引、WITHOUT ROWID、加密库、写操作。
package cookie

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"unicode/utf16"
)

// ---------------------------------------------------------------------------
// 值

// Kind 是单元格的类型。
type Kind int

const (
	KindNull Kind = iota
	KindInt
	KindFloat
	KindText
	KindBlob
)

// Value 是一个单元格。
type Value struct {
	Kind  Kind
	Int   int64
	Float float64
	Str   string
	Blob  []byte
}

// Text 把值当成字符串返回（BLOB 也按字节当字符串，Cookie 库里就是这样存的）。
func (v Value) Text() string {
	switch v.Kind {
	case KindText:
		return v.Str
	case KindBlob:
		return string(v.Blob)
	case KindInt:
		return fmt.Sprintf("%d", v.Int)
	default:
		return ""
	}
}

// Bytes 返回原始字节（TEXT 与 BLOB 都适用），用于解密。
func (v Value) Bytes() []byte {
	switch v.Kind {
	case KindText:
		return []byte(v.Str)
	case KindBlob:
		return v.Blob
	default:
		return nil
	}
}

// ---------------------------------------------------------------------------
// 页读取

// pager 把整个数据库读进内存，并按页号提供访问。
//
// 一次性读进内存的理由：Cookie 库通常只有几 MB，而 B-tree 遍历需要随机跳页，
// 逐页 seek 反而更麻烦。WAL 重放也需要在内存里改页。
type pager struct {
	data       []byte
	pageSize   int
	reserved   int // 每页尾部保留的字节数
	usableSize int // pageSize - reserved
	enc        int // 1=UTF-8, 2=UTF-16LE, 3=UTF-16BE
	pages      int
}

var errNotSQLite = errors.New("不是 SQLite 数据库（文件头不对）")

func openPager(path string) (*pager, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 100 {
		return nil, errNotSQLite
	}
	if !bytes.HasPrefix(raw, []byte("SQLite format 3\x00")) {
		return nil, errNotSQLite
	}

	p := &pager{data: raw}
	ps := int(binary.BigEndian.Uint16(raw[16:18]))
	if ps == 1 {
		ps = 65536
	}
	if ps < 512 || ps&(ps-1) != 0 {
		return nil, fmt.Errorf("页大小不合法：%d", ps)
	}
	p.pageSize = ps
	p.reserved = int(raw[20])
	p.usableSize = ps - p.reserved
	if p.usableSize < 480 {
		return nil, fmt.Errorf("可用页大小不合法：%d", p.usableSize)
	}
	p.enc = int(binary.BigEndian.Uint32(raw[56:60]))
	if p.enc == 0 {
		p.enc = 1 // 规范说 0 视为 UTF-8
	}
	p.pages = (len(raw) + ps - 1) / ps

	// WAL 里可能还有已提交但尚未回写主库的页（浏览器一直开着就常常如此）。
	// 不重放会读到过期的登录状态，所以这一步是必须的。
	if err := p.applyWAL(path + "-wal"); err != nil {
		return nil, err
	}
	return p, nil
}

// page 返回第 n 页（1 起）。返回值已经去掉尾部保留区。
func (p *pager) page(n int) ([]byte, error) {
	if n < 1 || n > p.pages {
		return nil, fmt.Errorf("页号越界：%d", n)
	}
	start := (n - 1) * p.pageSize
	end := start + p.pageSize
	if end > len(p.data) {
		return nil, fmt.Errorf("第 %d 页超出文件范围", n)
	}
	return p.data[start : start+p.usableSize], nil
}

// applyWAL 把 WAL 里**已提交**的帧重放到内存镜像上。
//
// WAL 格式（https://sqlite.org/fileformat2.html#walformat）：
//
//	32 字节头：magic(0x377f0682/83) 版本 页大小 检查点序号 盐 校验和
//	之后是若干帧：24 字节帧头（页号 / 提交时数据库页数 / 盐 / 校验和）+ 一页数据
//
// 判定「已提交」的简单可靠办法：一帧的「提交时数据库页数」不为 0 就表示
// 从 WAL 开头到这一帧是一个完整的提交。我们取**最后一个**这样的位置，
// 之后的帧都是未提交的（或者属于下一次事务），全部丢弃。
//
// 刻意不校验 checksum：只读导入场景下，校验失败最多是读到旧数据，
// 而为了校验要按 WAL 头里的字节序做一轮累加，收益不抵复杂度。
func (p *pager) applyWAL(walPath string) error {
	raw, err := os.ReadFile(walPath)
	if err != nil {
		// 没有 WAL 是常态（例如浏览器已退出并做了 checkpoint）。
		if os.IsNotExist(err) {
			return nil
		}
		return nil
	}
	const hdr = 32
	const frameHdr = 24
	if len(raw) < hdr+frameHdr {
		return nil
	}
	magic := binary.BigEndian.Uint32(raw[0:4])
	if magic != 0x377f0682 && magic != 0x377f0683 {
		return nil // 不是可识别的 WAL，忽略
	}
	walPageSize := int(binary.BigEndian.Uint32(raw[8:12]))
	if walPageSize != p.pageSize {
		return nil // 页大小对不上，宁可不重放也不要把数据搞乱
	}
	frameSize := frameHdr + walPageSize

	// 先找到最后一个提交点。
	last := -1
	for off := hdr; off+frameSize <= len(raw); off += frameSize {
		if binary.BigEndian.Uint32(raw[off+4:off+8]) != 0 {
			last = off
		}
	}
	if last < 0 {
		return nil // 没有任何已提交的事务
	}

	for off := hdr; off <= last; off += frameSize {
		pgno := int(binary.BigEndian.Uint32(raw[off : off+4]))
		if pgno < 1 {
			continue
		}
		src := raw[off+frameHdr : off+frameHdr+walPageSize]
		if err := p.writePage(pgno, src); err != nil {
			return err
		}
	}
	return nil
}

// writePage 把一页写进内存镜像，必要时扩展。
func (p *pager) writePage(n int, content []byte) error {
	start := (n - 1) * p.pageSize
	end := start + p.pageSize
	if end > len(p.data) {
		// 只有 WAL 里出现过更大的页号时才需要扩容；容量按需翻倍。
		grown := make([]byte, end)
		copy(grown, p.data)
		p.data = grown
	}
	copy(p.data[start:end], content)
	if n > p.pages {
		p.pages = n
	}
	return nil
}

// ---------------------------------------------------------------------------
// varint 与记录

// uvarint 读一个 SQLite 的 1~9 字节大端 varint，返回值和消耗的字节数。
func uvarint(b []byte) (uint64, int, error) {
	var v uint64
	for i := 0; i < 9 && i < len(b); i++ {
		c := b[i]
		if i == 8 {
			// 第 9 字节用满 8 位
			v = v<<8 | uint64(c)
			return v, 9, nil
		}
		v = v<<7 | uint64(c&0x7f)
		if c&0x80 == 0 {
			return v, i + 1, nil
		}
	}
	return 0, 0, errors.New("varint 不完整")
}

// decodeRecord 解码一条记录（payload）。
func (p *pager) decodeRecord(payload []byte) ([]Value, error) {
	hdrLen, n, err := uvarint(payload)
	if err != nil {
		return nil, err
	}
	if hdrLen < uint64(n) || hdrLen > uint64(len(payload)) {
		return nil, fmt.Errorf("记录头长度不合法：%d", hdrLen)
	}

	// 头部由「头长度」本身 + 各列的 serial type 组成，全是 varint。
	var serials []uint64
	pos := n
	for pos < int(hdrLen) {
		st, m, err := uvarint(payload[pos:])
		if err != nil {
			return nil, err
		}
		serials = append(serials, st)
		pos += m
	}

	// 值区从头部之后开始，按 serial type 依次取。
	out := make([]Value, 0, len(serials))
	body := int(hdrLen)
	for _, st := range serials {
		v, size, err := p.decodeValue(st, payload[body:])
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		body += size
	}
	return out, nil
}

// serialSize 返回某个 serial type 的值占多少字节。
func serialSize(st uint64) int {
	switch {
	case st == 0 || st == 8 || st == 9 || st == 10 || st == 11:
		return 0
	case st <= 4:
		return int(st)
	case st == 5:
		return 6
	case st == 6 || st == 7:
		return 8
	default:
		return int((st - 12) / 2)
	}
}

// decodeValue 按 serial type 解出一个值，并返回它占用的字节数。
func (p *pager) decodeValue(st uint64, b []byte) (Value, int, error) {
	size := serialSize(st)
	if size > len(b) {
		return Value{}, 0, fmt.Errorf("记录被截断：serial=%d 需要 %d 字节，只剩 %d", st, size, len(b))
	}
	raw := b[:size]

	switch {
	case st == 0:
		return Value{Kind: KindNull}, 0, nil
	case st >= 1 && st <= 6:
		// 1~6 字节大端有符号整数
		var v int64
		if size > 0 && raw[0]&0x80 != 0 {
			v = -1 // 负数：先全 1 再按字节补
		}
		for _, c := range raw {
			v = v<<8 | int64(c)
		}
		return Value{Kind: KindInt, Int: v}, size, nil
	case st == 7:
		bits := binary.BigEndian.Uint64(raw)
		return Value{Kind: KindFloat, Float: math.Float64frombits(bits)}, size, nil
	case st == 8:
		return Value{Kind: KindInt, Int: 0}, 0, nil
	case st == 9:
		return Value{Kind: KindInt, Int: 1}, 0, nil
	case st >= 12 && st%2 == 0:
		return Value{Kind: KindBlob, Blob: append([]byte(nil), raw...)}, size, nil
	case st >= 13 && st%2 == 1:
		return Value{Kind: KindText, Str: p.decodeText(raw)}, size, nil
	default:
		// 10/11 是保留值，按 NULL 处理而不是报错，避免一条怪数据毁掉整张表。
		return Value{Kind: KindNull}, 0, nil
	}
}

// decodeText 按数据库的文本编码把字节转成字符串。
func (p *pager) decodeText(b []byte) string {
	switch p.enc {
	case 2: // UTF-16LE
		return utf16ToString(b, binary.LittleEndian)
	case 3: // UTF-16BE
		return utf16ToString(b, binary.BigEndian)
	default:
		return string(b)
	}
}

// ---------------------------------------------------------------------------
// 表遍历

// 溢出阈值。这几个常数的来源是 SQLite 的 payload 溢出算法：
// 表叶子页的 maxLocal = usableSize - 35，minLocal = (usableSize-12)*32/255 - 23。
const (
	tableLeafOverhead     = 35
	tableInteriorOverhead = 12
)

func (p *pager) maxLocalTableLeaf() int {
	return p.usableSize - tableLeafOverhead
}

func (p *pager) minLocalTableLeaf() int {
	return (p.usableSize-tableInteriorOverhead)*32/255 - 23
}

// readTable 从根页开始遍历整张表，逐行回调。
func (p *pager) readTable(root int, fn func([]Value) error) error {
	visited := map[int]bool{}
	return p.walkTable(root, visited, fn)
}

func (p *pager) walkTable(pgno int, visited map[int]bool, fn func([]Value) error) error {
	if visited[pgno] {
		return fmt.Errorf("B-tree 出现环：页 %d 被重复访问", pgno)
	}
	visited[pgno] = true

	pg, err := p.page(pgno)
	if err != nil {
		return err
	}

	// 第 1 页前面压着 100 字节的文件头，B-tree 页头要从 100 开始读。
	// 这里踩过一次坑：只把「单元指针数组」的偏移加了 100，却漏了页类型与单元数，
	// 于是第 1 页永远被当成未知类型。造库测试当场就抓出来了。
	hdrOff := 0
	if pgno == 1 {
		hdrOff = 100
	}
	if len(pg) < hdrOff+8 {
		return fmt.Errorf("页 %d 太短", pgno)
	}

	typ := pg[hdrOff]
	ncells := int(binary.BigEndian.Uint16(pg[hdrOff+3 : hdrOff+5]))

	cellPtrs := hdrOff + 8
	if typ == 5 { // 内部页的表要跳过 4 字节最右指针
		cellPtrs = hdrOff + 12
	}

	switch typ {
	case 13: // 表叶子页
		for i := 0; i < ncells; i++ {
			off := cellPtrs + i*2
			if off+2 > len(pg) {
				return fmt.Errorf("页 %d 的单元指针数组越界", pgno)
			}
			cp := int(binary.BigEndian.Uint16(pg[off : off+2]))
			if cp < 0 || cp >= len(pg) {
				return fmt.Errorf("页 %d 的单元偏移越界：%d", pgno, cp)
			}
			payload, err := p.leafCellPayload(pg, cp)
			if err != nil {
				return err
			}
			rec, err := p.decodeRecord(payload)
			if err != nil {
				return err
			}
			if err := fn(rec); err != nil {
				return err
			}
		}
		return nil

	case 5: // 表内部页
		for i := 0; i < ncells; i++ {
			off := cellPtrs + i*2
			if off+2 > len(pg) {
				return fmt.Errorf("页 %d 的单元指针数组越界", pgno)
			}
			cp := int(binary.BigEndian.Uint16(pg[off : off+2]))
			if cp+4 > len(pg) {
				return fmt.Errorf("页 %d 的内部单元越界：%d", pgno, cp)
			}
			child := int(binary.BigEndian.Uint32(pg[cp : cp+4]))
			if err := p.walkTable(child, visited, fn); err != nil {
				return err
			}
		}
		// 最后走最右子页
		right := int(binary.BigEndian.Uint32(pg[hdrOff+8 : hdrOff+12]))
		return p.walkTable(right, visited, fn)

	default:
		// 索引页（2/10）不该出现在表遍历里。
		return fmt.Errorf("页 %d 的类型是 %d，不是表 B-tree 页", pgno, typ)
	}
}

// leafCellPayload 取出表叶子页里某个单元的完整 payload，必要时拼接溢出页。
func (p *pager) leafCellPayload(pg []byte, cellOff int) ([]byte, error) {
	payloadLen, n, err := uvarint(pg[cellOff:])
	if err != nil {
		return nil, err
	}
	// rowid 也要跳过
	_, rn, err := uvarint(pg[cellOff+n:])
	if err != nil {
		return nil, err
	}
	local := cellOff + n + rn

	maxLocal := p.maxLocalTableLeaf()
	minLocal := p.minLocalTableLeaf()
	total := int(payloadLen)

	// 不溢出：payload 全在页里。
	if total <= maxLocal {
		if local+total > len(pg) {
			return nil, fmt.Errorf("单元 payload 越界")
		}
		return pg[local : local+total], nil
	}

	// 溢出：先算出页内保留多少字节。
	var inline int
	k := minLocal + (total-minLocal)%(p.usableSize-4)
	if k <= maxLocal {
		inline = k
	} else {
		inline = minLocal
	}
	if local+inline+4 > len(pg) {
		return nil, fmt.Errorf("溢出单元的页内部分越界")
	}

	out := make([]byte, 0, total)
	out = append(out, pg[local:local+inline]...)

	next := int(binary.BigEndian.Uint32(pg[local+inline : local+inline+4]))
	remain := total - inline
	guard := 0
	for next != 0 && remain > 0 {
		if guard++; guard > 100000 {
			return nil, fmt.Errorf("溢出页链过长，可能已损坏")
		}
		op, err := p.page(next)
		if err != nil {
			return nil, err
		}
		if len(op) < 4 {
			return nil, fmt.Errorf("溢出页 %d 太短", next)
		}
		chunk := p.usableSize - 4
		if chunk > remain {
			chunk = remain
		}
		if 4+chunk > len(op) {
			return nil, fmt.Errorf("溢出页 %d 数据越界", next)
		}
		out = append(out, op[4:4+chunk]...)
		remain -= chunk
		next = int(binary.BigEndian.Uint32(op[0:4]))
	}
	if remain != 0 {
		return nil, fmt.Errorf("溢出页链提前结束，还差 %d 字节", remain)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 对外接口

// Table 是一张读出来的表。
type Table struct {
	Name string
	Cols []string
	Rows [][]Value
}

// Column 返回某一列的下标，找不到返回 -1。
func (t *Table) Column(name string) int {
	for i, c := range t.Cols {
		if strings.EqualFold(c, name) {
			return i
		}
	}
	return -1
}

// ReadTable 打开一个 SQLite 文件并把指定表整张读出来。
//
// 只做读、只做表 B-tree，够用即可：我们要的只是浏览器的 cookie 表。
func ReadTable(path, table string) (*Table, error) {
	p, err := openPager(path)
	if err != nil {
		return nil, err
	}

	// 先从 sqlite_master（根页固定是 1）里找到目标表的根页与建表语句。
	var rootPage int
	var createSQL string
	err = p.readTable(1, func(rec []Value) error {
		if len(rec) < 5 {
			return nil
		}
		if !strings.EqualFold(rec[0].Text(), "table") {
			return nil
		}
		if !strings.EqualFold(rec[1].Text(), table) {
			return nil
		}
		rootPage = int(rec[3].Int)
		createSQL = rec[4].Text()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("读取 sqlite_master 失败：%w", err)
	}
	if rootPage == 0 {
		return nil, fmt.Errorf("库里没有表 %q", table)
	}

	t := &Table{Name: table, Cols: parseColumnNames(createSQL)}

	err = p.readTable(rootPage, func(rec []Value) error {
		t.Rows = append(t.Rows, rec)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("读取表 %q 失败：%w", table, err)
	}

	// 万一建表语句解析不出列名（非常规写法），退回按第一行的宽度编号，
	// 至少让调用方能用下标取值，而不是整个功能报废。
	if len(t.Cols) == 0 && len(t.Rows) > 0 {
		for i := range t.Rows[0] {
			t.Cols = append(t.Cols, fmt.Sprintf("col%d", i))
		}
	}
	return t, nil
}

// parseColumnNames 从 CREATE TABLE 语句里抽出列名。
//
// 只解析最外层括号里的内容，按顶层逗号切分，取每段的第一个词。
// 表级约束（PRIMARY KEY(...) / UNIQUE(...) / FOREIGN KEY(...)）会被跳过。
func parseColumnNames(sql string) []string {
	open := strings.IndexByte(sql, '(')
	if open < 0 {
		return nil
	}
	depth := 0
	end := -1
	for i := open; i < len(sql); i++ {
		switch sql[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return nil
	}

	var out []string
	for _, part := range splitTopLevel(sql[open+1 : end]) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name := firstToken(part)
		if name == "" {
			continue
		}
		// 表级约束不是列。
		switch strings.ToUpper(name) {
		case "PRIMARY", "UNIQUE", "CHECK", "FOREIGN", "CONSTRAINT":
			continue
		}
		out = append(out, unquoteIdent(name))
	}
	return out
}

// splitTopLevel 按顶层逗号切分（括号内的逗号不算）。
func splitTopLevel(s string) []string {
	var out []string
	depth := 0
	start := 0
	inQuote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote != 0 {
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			inQuote = c
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}

// firstToken 取一段文本里第一个标识符（可能被引号包着）。
func firstToken(s string) string {
	s = strings.TrimLeft(s, " \t\r\n")
	if s == "" {
		return ""
	}
	if c := s[0]; c == '"' || c == '`' || c == '[' {
		closer := byte('"')
		if c == '`' {
			closer = '`'
		} else if c == '[' {
			closer = ']'
		}
		if i := strings.IndexByte(s[1:], closer); i >= 0 {
			return s[:i+2]
		}
		return s
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r', '\n', '(':
			return s[:i]
		}
	}
	return s
}

// unquoteIdent 去掉标识符外面的引号。
func unquoteIdent(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') ||
			(s[0] == '`' && s[len(s)-1] == '`') ||
			(s[0] == '[' && s[len(s)-1] == ']') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// utf16ToString 按指定字节序把 UTF-16 字节转成字符串。
func utf16ToString(b []byte, order binary.ByteOrder) string {
	if len(b)%2 != 0 {
		b = b[:len(b)-1] // 尾字节残缺就丢掉，不值得为它报错
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, order.Uint16(b[i:i+2]))
	}
	return string(utf16.Decode(u))
}
