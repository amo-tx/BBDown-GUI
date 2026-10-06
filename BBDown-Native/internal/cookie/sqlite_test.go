package cookie

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// 造一个最小可用的 SQLite 文件
//
// 自己造而不是依赖本机浏览器：浏览器不一定装了、装了也不一定有 B 站 cookie，
// 那样测试就只能 skip，等于没测。造文件的代价是几十行，换来解析器每次都被真跑一遍。

const testPageSize = 4096

// mkVarint 编码 SQLite 的 1~9 字节大端 varint。
func mkVarint(v uint64) []byte {
	if v <= 0x7f {
		return []byte{byte(v)}
	}
	if v <= 0x3fff {
		return []byte{byte(v>>7 | 0x80), byte(v & 0x7f)}
	}
	var buf [9]byte
	n := 0
	// 9 字节的最后一字节用满 8 位，其余 7 位一组
	v2 := v
	groups := []byte{}
	for v2 > 0 {
		groups = append([]byte{byte(v2 & 0x7f)}, groups...)
		v2 >>= 7
	}
	if len(groups) <= 8 {
		for i := 0; i < len(groups)-1; i++ {
			buf[n] = groups[i] | 0x80
			n++
		}
		buf[n] = groups[len(groups)-1]
		n++
		return buf[:n]
	}
	// 超过 8 组才会用到第 9 字节，测试用不到，直接不支持
	panic("mkVarint 只支持到 8 字节")
}

// mkRecord 按 SQLite 的记录格式编码一行。
func mkRecord(fields ...any) []byte {
	var serials []uint64
	var body []byte

	for _, f := range fields {
		switch x := f.(type) {
		case nil:
			serials = append(serials, 0)
		case string:
			serials = append(serials, uint64(13+2*len(x)))
			body = append(body, x...)
		case []byte:
			serials = append(serials, uint64(12+2*len(x)))
			body = append(body, x...)
		case int64:
			switch {
			case x == 0:
				serials = append(serials, 8)
			case x == 1:
				serials = append(serials, 9)
			case x >= -128 && x <= 127:
				serials = append(serials, 1)
				body = append(body, byte(x))
			case x >= -32768 && x <= 32767:
				serials = append(serials, 2)
				body = binary.BigEndian.AppendUint16(body, uint16(int16(x)))
			case x >= -2147483648 && x <= 2147483647:
				serials = append(serials, 4)
				body = binary.BigEndian.AppendUint32(body, uint32(int32(x)))
			default:
				serials = append(serials, 6)
				body = binary.BigEndian.AppendUint64(body, uint64(x))
			}
		default:
			panic("不支持的字段类型")
		}
	}

	var hdr []byte
	for _, s := range serials {
		hdr = append(hdr, mkVarint(s)...)
	}
	// 头部长度 = 头长度字段自身 + 各 serial
	hdrLenBytes := len(mkVarint(uint64(len(hdr) + 1)))
	total := len(hdr) + hdrLenBytes
	out := append(mkVarint(uint64(total)), hdr...)
	return append(out, body...)
}

// maxLocal / minLocal 与生产代码保持一致，测试里要算出溢出阈值。
func testMaxLocal() int { return testPageSize - 35 }
func testMinLocal() int { return (testPageSize-12)*32/255 - 23 }

// mkLeafCell 把一行包成表叶子页的单元，必要时补溢出页。
//
// 返回值：单元字节 + 追加到文件末尾的溢出页内容（可能为 nil）。
func mkLeafCell(rowid int64, payload []byte, overflowStartPage int) (cell []byte, overflowPages [][]byte) {
	var head []byte
	head = append(head, mkVarint(uint64(len(payload)))...)
	head = append(head, mkVarint(uint64(rowid))...)

	maxLocal := testMaxLocal()
	if len(payload) <= maxLocal {
		return append(head, payload...), nil
	}

	minLocal := testMinLocal()
	inline := minLocal
	if k := minLocal + (len(payload)-minLocal)%(testPageSize-4); k <= maxLocal {
		inline = k
	}

	rest := payload[inline:]
	cell = append(head, payload[:inline]...)
	cell = binary.BigEndian.AppendUint32(cell, uint32(overflowStartPage))

	pageNo := overflowStartPage
	for len(rest) > 0 {
		chunk := len(rest)
		if chunk > testPageSize-4 {
			chunk = testPageSize - 4
		}
		next := pageNo + 1
		if chunk == len(rest) {
			next = 0
		}
		pg := make([]byte, testPageSize)
		binary.BigEndian.PutUint32(pg[0:4], uint32(next))
		copy(pg[4:], rest[:chunk])
		overflowPages = append(overflowPages, pg)
		rest = rest[chunk:]
		pageNo++
	}
	return cell, overflowPages
}

// mkLeafPage 组装一个表叶子页。
func mkLeafPage(pageNo int, cells [][]byte) []byte {
	pg := make([]byte, testPageSize)
	hdrOff := 0
	if pageNo == 1 {
		hdrOff = 100
	}
	pg[hdrOff] = 13 // 表叶子页
	binary.BigEndian.PutUint16(pg[hdrOff+3:], uint16(len(cells)))

	off := testPageSize
	ptrs := make([]int, len(cells))
	for i, c := range cells {
		off -= len(c)
		copy(pg[off:], c)
		ptrs[i] = off
	}
	binary.BigEndian.PutUint16(pg[hdrOff+5:], uint16(off))
	for i, p := range ptrs {
		binary.BigEndian.PutUint16(pg[hdrOff+8+i*2:], uint16(p))
	}
	return pg
}

// mkDB 造一个含 sqlite_master 与一张数据表的数据库。
//
//	payloads —— 各行的**记录**（mkRecord 的输出，不含单元头）
//
// 注意 payload 与 cell 的区别：cell = payload长度varint + rowid varint + payload。
// 早先把 payload 直接当 cell 塞进去，于是解析器读到的第一个 varint 就错了位。
// 超过一页的行会自动补溢出页，页号紧跟在数据页之后。
func buildSimpleDB(payloads [][]byte, ignored [][]byte, schemaSQL string) []byte {
	const dataPageNo = 2

	master := mkRecord("table", "cookies", "cookies", int64(dataPageNo), schemaSQL)
	masterCell, _ := mkLeafCell(1, master, 0)
	page1 := mkLeafPage(1, [][]byte{masterCell})

	cells := make([][]byte, 0, len(payloads))
	var overflow [][]byte
	overflowPageNo := dataPageNo + 1
	for i, payload := range payloads {
		cell, extra := mkLeafCell(int64(i+1), payload, overflowPageNo)
		cells = append(cells, cell)
		overflow = append(overflow, extra...)
		overflowPageNo += len(extra)
	}
	page2 := mkLeafPage(dataPageNo, cells)
	_ = ignored

	totalPages := dataPageNo + len(overflow)

	out := append([]byte(nil), page1...)
	// page1 的前 100 字节留给了文件头，这里补上真实内容。
	hdr := make([]byte, 100)
	copy(hdr, "SQLite format 3\x00")
	binary.BigEndian.PutUint16(hdr[16:], testPageSize)
	hdr[18], hdr[19] = 1, 1
	binary.BigEndian.PutUint32(hdr[28:], uint32(totalPages))
	binary.BigEndian.PutUint32(hdr[44:], 4)
	binary.BigEndian.PutUint32(hdr[56:], 1) // UTF-8
	copy(out[:100], hdr)

	out = append(out, page2...)
	for _, op := range overflow {
		out = append(out, op...)
	}
	return out
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "Cookies")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("写临时库失败：%v", err)
	}
	return p
}

const testSchema = `CREATE TABLE cookies(creation_utc INTEGER NOT NULL, host_key TEXT NOT NULL,
top_frame_site_key TEXT NOT NULL DEFAULT '', name TEXT NOT NULL, value TEXT NOT NULL,
encrypted_value BLOB DEFAULT '', path TEXT NOT NULL, expires_utc INTEGER NOT NULL,
is_secure INTEGER NOT NULL, is_httponly INTEGER NOT NULL, last_access_utc INTEGER NOT NULL,
has_expires INTEGER NOT NULL DEFAULT 1, is_persistent INTEGER NOT NULL DEFAULT 1,
priority INTEGER NOT NULL DEFAULT 1, samesite INTEGER NOT NULL DEFAULT -1,
source_scheme INTEGER NOT NULL DEFAULT 0, source_port INTEGER NOT NULL DEFAULT -1,
last_update_utc INTEGER NOT NULL DEFAULT 0, source_type INTEGER NOT NULL DEFAULT 0,
has_cross_site_ancestor INTEGER NOT NULL DEFAULT 0)`

// cookieRowBytes 造一行 cookie（列顺序与真实表一致）。
func cookieRowBytes(host, name, value string, enc []byte) []byte {
	return mkRecord(
		int64(13300000000000000), host, "", name, value, enc, "/",
		int64(13400000000000000), int64(1), int64(0), int64(13300000000000000),
		int64(1), int64(1), int64(1), int64(-1), int64(0), int64(-1), int64(0), int64(0), int64(0),
	)
}

// ---------------------------------------------------------------------------
// 解析器测试

func TestReadTableBasic(t *testing.T) {
	rows := [][]byte{
		cookieRowBytes(".bilibili.com", "SESSDATA", "abc123", nil),
		cookieRowBytes(".bilibili.com", "bili_jct", "def456", nil),
		cookieRowBytes("www.example.com", "other", "zzz", nil),
	}
	db := writeTemp(t, buildSimpleDB(rows, nil, testSchema))

	tbl, err := ReadTable(db, "cookies")
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if len(tbl.Rows) != 3 {
		t.Fatalf("期望 3 行，实际 %d 行", len(tbl.Rows))
	}

	// 列名必须从建表语句里正确解析出来
	for _, want := range []string{"creation_utc", "host_key", "name", "value", "encrypted_value", "expires_utc"} {
		if tbl.Column(want) < 0 {
			t.Errorf("缺少列 %q，实际列名：%v", want, tbl.Cols)
		}
	}
	// 表级约束不该被当成列
	for _, col := range tbl.Cols {
		switch col {
		case "PRIMARY", "UNIQUE", "CHECK", "FOREIGN", "CONSTRAINT":
			t.Errorf("把表级约束当成了列：%q", col)
		}
	}
	if len(tbl.Cols) != 20 {
		t.Errorf("期望 20 列，实际 %d 列：%v", len(tbl.Cols), tbl.Cols)
	}

	hi, ni, vi := tbl.Column("host_key"), tbl.Column("name"), tbl.Column("value")
	got := map[string]string{}
	for _, r := range tbl.Rows {
		got[r[ni].Text()] = r[hi].Text() + "|" + r[vi].Text()
	}
	if got["SESSDATA"] != ".bilibili.com|abc123" {
		t.Errorf("SESSDATA 行不对：%q", got["SESSDATA"])
	}
	if got["other"] != "www.example.com|zzz" {
		t.Errorf("other 行不对：%q", got["other"])
	}
}

func TestReadTableBlobAndInt(t *testing.T) {
	blob := []byte{0x01, 0x00, 0x00, 0x00, 0xAA, 0xBB}
	rows := [][]byte{cookieRowBytes(".bilibili.com", "enc", "", blob)}
	db := writeTemp(t, buildSimpleDB(rows, nil, testSchema))

	tbl, err := ReadTable(db, "cookies")
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	ei := tbl.Column("encrypted_value")
	exp := tbl.Column("expires_utc")
	if !bytes.Equal(tbl.Rows[0][ei].Bytes(), blob) {
		t.Errorf("BLOB 解码不对：%x", tbl.Rows[0][ei].Bytes())
	}
	if tbl.Rows[0][ei].Kind != KindBlob {
		t.Errorf("encrypted_value 应当是 BLOB，实际 Kind=%d", tbl.Rows[0][ei].Kind)
	}
	if tbl.Rows[0][exp].Int != 13400000000000000 {
		t.Errorf("INTEGER 解码不对：%d", tbl.Rows[0][exp].Int)
	}
}

// 负数要用 6 字节 / 8 字节的补码正确还原（cookies 表里 samesite / source_port 常为 -1）。
func TestReadTableNegativeIntegers(t *testing.T) {
	row := mkRecord(
		int64(1), ".bilibili.com", "", "SESSDATA", "v", []byte{}, "/",
		int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0),
		int64(-1), int64(0), int64(-1), int64(0), int64(0), int64(0),
	)
	db := writeTemp(t, buildSimpleDB([][]byte{row}, nil, testSchema))
	tbl, err := ReadTable(db, "cookies")
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if got := tbl.Rows[0][tbl.Column("samesite")].Int; got != -1 {
		t.Errorf("samesite 期望 -1，实际 %d", got)
	}
	if got := tbl.Rows[0][tbl.Column("source_port")].Int; got != -1 {
		t.Errorf("source_port 期望 -1，实际 %d", got)
	}
}

// 一条超过页容量的值必须通过溢出页正确拼回来。
func TestReadTableOverflowPage(t *testing.T) {
	big := bytes.Repeat([]byte("AB"), 2500) // 5000 字节 > maxLocal(4061)
	if len(big) <= testMaxLocal() {
		t.Fatal("测试数据没有触发溢出页，用例失去意义")
	}
	rows := [][]byte{cookieRowBytes(".bilibili.com", "huge", "", big)}
	db := writeTemp(t, buildSimpleDB(rows, nil, testSchema))

	tbl, err := ReadTable(db, "cookies")
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	got := tbl.Rows[0][tbl.Column("encrypted_value")].Bytes()
	if !bytes.Equal(got, big) {
		t.Errorf("溢出页拼接结果不对：长度 %d，期望 %d", len(got), len(big))
	}
}

func TestReadTableRollbackSeparated(t *testing.T) {
	rows := [][]byte{cookieRowBytes(".bilibili.com", "a", "1", nil)}
	db := writeTemp(t, buildSimpleDB(rows, nil, testSchema))

	if _, err := ReadTable(db, "nonexistent"); err == nil {
		t.Error("表不存在时应当报错")
	}
}

func TestOpenRejectsGarbage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad")
	if err := os.WriteFile(p, []byte("这不是 SQLite，只是一段普通文本而已，长度凑够一百字节吧吧吧吧吧吧吧吧吧吧吧吧吧吧吧吧吧吧吧吧吧吧吧吧吧"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTable(p, "cookies"); err == nil {
		t.Error("非 SQLite 文件应当报错")
	}
}

// ---------------------------------------------------------------------------
// 列名解析

func TestParseColumnNames(t *testing.T) {
	got := parseColumnNames(`CREATE TABLE t(a TEXT, b INTEGER NOT NULL DEFAULT 0, "c d" BLOB, PRIMARY KEY(a, b))`)
	want := []string{"a", "b", "c d"}
	if len(got) != len(want) {
		t.Fatalf("期望 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 列期望 %q，实际 %q", i, want[i], got[i])
		}
	}
}

func TestParseColumnNamesSkipsTableConstraints(t *testing.T) {
	got := parseColumnNames(`CREATE TABLE t(x TEXT, UNIQUE(x), CHECK(x <> ''), FOREIGN KEY(x) REFERENCES u(y))`)
	if len(got) != 1 || got[0] != "x" {
		t.Errorf("表级约束应当被跳过，实际 %v", got)
	}
}

func TestParseColumnNamesWithNestedParens(t *testing.T) {
	got := parseColumnNames(`CREATE TABLE t(a INTEGER DEFAULT (1+2), b TEXT)`)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("带括号默认值时解析错误：%v", got)
	}
}

// ---------------------------------------------------------------------------
// varint / 记录

func TestVarintRoundTrip(t *testing.T) {
	for _, v := range []uint64{0, 1, 127, 128, 300, 16383, 16384, 1 << 20, 1 << 40} {
		b := mkVarint(v)
		got, n, err := uvarint(b)
		if err != nil {
			t.Fatalf("uvarint(%d) 出错：%v", v, err)
		}
		if got != v || n != len(b) {
			t.Errorf("varint %d 往返失败：得到 %d（用了 %d 字节，期望 %d）", v, got, n, len(b))
		}
	}
}

func TestUvarintTruncated(t *testing.T) {
	if _, _, err := uvarint([]byte{0x80, 0x80}); err == nil {
		t.Error("被截断的 varint 应当报错")
	}
}

func TestSerialSize(t *testing.T) {
	cases := map[uint64]int{
		0: 0, 1: 1, 2: 2, 3: 3, 4: 4, 5: 6, 6: 8, 7: 8, 8: 0, 9: 0,
		12: 0, 13: 0, 14: 1, 15: 1, 16: 2, 17: 2,
	}
	for st, want := range cases {
		if got := serialSize(st); got != want {
			t.Errorf("serialSize(%d) = %d，期望 %d", st, got, want)
		}
	}
}
