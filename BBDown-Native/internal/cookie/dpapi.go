//go:build windows

package cookie

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

// DPAPI（CryptProtectData / CryptUnprotectData）的调用。
//
// 先说清楚这里为什么用 unsafe、以及为什么 go vet 依然干净 ——
// 这一条踩过坑，别照抄网上的写法：
//
//	CryptUnprotectData 的输出是**它自己用 LocalAlloc 分配的**缓冲区，
//	想把内容读回 Go 就得把拿到的指针转成 Go 指针。
//	而 `uintptr → unsafe.Pointer` 正是 go vet 的 unsafeptr 检查会拦下的方向
//	（实测三种写法：存变量、直接内联、经函数中转，全部报 possible misuse）。
//
//	解法是让内核替我们搬：
//	    ReadProcessMemory(GetCurrentProcess(), C指针, Go缓冲区, 长度, &已读)
//	C 指针在这儿只是一个整数实参，**全程不做那次转换**；
//	Go 缓冲区那边是 `uintptr(unsafe.Pointer(&buf[0]))`，属于合法方向。
//	于是 vet 通过，代码也确实是安全的（目标就是本进程的堆）。
//
// 这条路还顺带绕开了「必须起一个 PowerShell 子进程」的方案 ——
// 那种方案会把待解密的密钥写进命令行，且依赖 .NET 的 ProtectedData 类。

// dataBlob 对应 Win32 的 DATA_BLOB：
//
//	typedef struct _CRYPTOAPI_BLOB { DWORD cbData; BYTE *pbData; } DATA_BLOB;
//
// x64 上指针要求 8 字节对齐，所以 cbData 后面有 4 字节填充，结构体共 16 字节。
type dataBlob struct {
	cbData uint32
	_      uint32
	pbData uintptr
}

var (
	crypt32            = syscall.NewLazyDLL("crypt32.dll")
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procProtect        = crypt32.NewProc("CryptProtectData")
	procUnprotect      = crypt32.NewProc("CryptUnprotectData")
	procReadProcessMem = kernel32.NewProc("ReadProcessMemory")
	procGetCurrentProc = kernel32.NewProc("GetCurrentProcess")
)

// CRYPTPROTECT_UI_FORBIDDEN：不要在界面上弹任何提示框（后台服务里必须加）。
const cryptprotectUIForbidden = 0x1

// ErrDPAPI 表示 DPAPI 调用失败。最常见的原因是「这份数据不是当前用户加密的」。
var ErrDPAPI = errors.New("DPAPI 解密失败")

// readProcBuffer 把 C 侧分配的缓冲区拷进 Go 切片。
//
// n 为 0 时直接返回 nil —— Win32 不喜欢长度为 0 的缓冲区。
func readProcBuffer(addr uintptr, n uint32) ([]byte, error) {
	if n == 0 || addr == 0 {
		return nil, nil
	}
	buf := make([]byte, n)
	var got uintptr
	cur, _, _ := procGetCurrentProc.Call()
	ok, _, err := procReadProcessMem.Call(
		cur,
		addr,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(n),
		uintptr(unsafe.Pointer(&got)),
	)
	if ok == 0 {
		return nil, fmt.Errorf("%w：ReadProcessMemory 失败（%v）", ErrDPAPI, err)
	}
	return buf[:got], nil
}

// dpapiUnprotect 用当前用户的密钥解密一段 DPAPI 密文。
func dpapiUnprotect(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, nil
	}
	in := dataBlob{cbData: uint32(len(blob)), pbData: uintptr(unsafe.Pointer(&blob[0]))}
	var out dataBlob

	ok, _, callErr := procUnprotect.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // ppszDataDescr：不要描述串
		0, // pOptionalEntropy：没有附加熵
		0, // pvReserved：保留，必须为 0
		0, // pPromptStruct：不要提示界面
		cryptprotectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	if ok == 0 {
		return nil, fmt.Errorf("%w：CryptUnprotectData 返回失败（%v）", ErrDPAPI, callErr)
	}
	// LocalAlloc 出来的内存必须 LocalFree，否则每导一次 cookie 就漏一块。
	defer syscall.LocalFree(syscall.Handle(out.pbData))

	return readProcBuffer(out.pbData, out.cbData)
}

// dpapiProtect 是 dpapiUnprotect 的反操作，只用于自检。
func dpapiProtect(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, nil
	}
	in := dataBlob{cbData: uint32(len(plain)), pbData: uintptr(unsafe.Pointer(&plain[0]))}
	var out dataBlob

	ok, _, callErr := procProtect.Call(
		uintptr(unsafe.Pointer(&in)),
		0, 0, 0, 0,
		cryptprotectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	if ok == 0 {
		return nil, fmt.Errorf("%w：CryptProtectData 返回失败（%v）", ErrDPAPI, callErr)
	}
	defer syscall.LocalFree(syscall.Handle(out.pbData))

	return readProcBuffer(out.pbData, out.cbData)
}
