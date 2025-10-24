package bpf

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go@v0.15.0 tycho ../../bpf/tycho.bpf.c -- -I../../bpf/include
