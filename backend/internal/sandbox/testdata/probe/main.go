package main

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func readLimit(name string) string {
	value, _ := os.ReadFile("/sys/fs/cgroup/" + name)
	return strings.TrimSpace(string(value))
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "child" {
		time.Sleep(90 * time.Second)
		return
	}
	input, err := io.ReadAll(io.LimitReader(os.Stdin, 32))
	if err != nil {
		os.Exit(1)
	}
	if string(input) == "hang" {
		child := exec.Command("/usr/local/bin/autolife-inspect", "child")
		if child.Start() != nil {
			os.Exit(1)
		}
		_ = child.Wait()
		os.Exit(1)
	}
	status, _ := os.ReadFile("/proc/self/status")
	lsm, _ := os.ReadFile("/proc/self/attr/current")
	interfaces, _ := net.Interfaces()
	isolated := len(interfaces) == 1 && interfaces[0].Flags&net.FlagLoopback != 0
	var info syscall.Statfs_t
	boundedDisk := syscall.Statfs("/work", &info) == nil && int64(info.Blocks)*info.Bsize <= 128<<20
	readOnlyBase := syscall.Statfs("/", &info) == nil && info.Flags&syscall.MS_RDONLY != 0
	memory, _ := strconv.ParseInt(readLimit("memory.max"), 10, 64)
	swap := readLimit("memory.swap.max")
	pids := readLimit("pids.max")
	cpu := strings.Fields(readLimit("cpu.max"))
	boundedCPU := len(cpu) == 2 && cpu[0] != "max" && cpu[0] == cpu[1]
	_, dockerErr := os.Stat("/var/run/docker.sock")
	_, markerErr := os.Stat("/tmp/autolife-host-probe-marker")
	rootWrite := os.WriteFile("/probe-writable/root-write", []byte("x"), 0600)
	workWrite := os.WriteFile("/work/test", []byte("x"), 0600)
	if os.Geteuid() == 0 || !strings.Contains(string(status), "NoNewPrivs:\t1") ||
		!strings.Contains(string(status), "Seccomp:\t2") || !strings.Contains(string(status), "CapEff:\t0000000000000000") ||
		!strings.Contains(string(lsm), "docker-default (enforce)") || !isolated || !boundedDisk ||
		!readOnlyBase || !boundedCPU || memory < 1 || memory > 512<<20 || swap != "0" || pids != "32" ||
		!os.IsNotExist(dockerErr) || !os.IsNotExist(markerErr) || rootWrite == nil || workWrite != nil {
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]bool{"confined": true})
}
