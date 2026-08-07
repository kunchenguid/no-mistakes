//go:build windows

package ipc

import (
	"encoding/binary"
	"math/bits"
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
)

const tcpTableOwnerPIDAll = 5

var getExtendedTCPTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

func authenticatedPeerPID(conn net.Conn) int {
	local, lok := conn.LocalAddr().(*net.TCPAddr)
	remote, rok := conn.RemoteAddr().(*net.TCPAddr)
	if !lok || !rok || local.IP.To4() == nil || remote.IP.To4() == nil {
		return 0
	}
	var size uint32
	ret, _, _ := getExtendedTCPTable.Call(0, uintptr(unsafe.Pointer(&size)), 0, windows.AF_INET, tcpTableOwnerPIDAll, 0)
	if ret != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) || size < 4 {
		return 0
	}
	buf := make([]byte, size)
	ret, _, _ = getExtendedTCPTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, windows.AF_INET, tcpTableOwnerPIDAll, 0)
	if ret != 0 {
		return 0
	}
	count := binary.LittleEndian.Uint32(buf[:4])
	const rowSize = 24 // six DWORDs in MIB_TCPROW_OWNER_PID
	localAddr := binary.LittleEndian.Uint32(local.IP.To4())
	remoteAddr := binary.LittleEndian.Uint32(remote.IP.To4())
	for idx := uint32(0); idx < count; idx++ {
		offset := 4 + int(idx)*rowSize
		if offset+rowSize > len(buf) {
			break
		}
		row := buf[offset : offset+rowSize]
		rowLocalAddr := binary.LittleEndian.Uint32(row[4:8])
		rowLocalPort := binary.LittleEndian.Uint32(row[8:12])
		rowRemoteAddr := binary.LittleEndian.Uint32(row[12:16])
		rowRemotePort := binary.LittleEndian.Uint32(row[16:20])
		// Match the reverse (client-owned) row, not the accepted socket's
		// server-owned row.
		if rowLocalAddr == remoteAddr && rowRemoteAddr == localAddr &&
			int(bits.ReverseBytes16(uint16(rowLocalPort))) == remote.Port &&
			int(bits.ReverseBytes16(uint16(rowRemotePort))) == local.Port {
			return int(binary.LittleEndian.Uint32(row[20:24]))
		}
	}
	return 0
}
