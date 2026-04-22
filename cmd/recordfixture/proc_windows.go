//go:build windows

package main

import "syscall"

func newProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}
