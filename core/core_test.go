package core

import (
	"reflect"
	"testing"
)

func TestKernelCapabilitiesContract(t *testing.T) {
	want := []KernelCapability{
		{Name: "juicity", Protocols: []string{"juicity"}},
		{Name: "mieru", Protocols: []string{"mieru"}},
		{Name: "naive", Protocols: []string{"naive"}},
		{Name: "sudoku", Protocols: []string{"sudoku"}},
	}

	if got := KernelCapabilities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("KernelCapabilities() = %#v, want %#v", got, want)
	}

	tests := []struct {
		name     string
		kernel   string
		protocol string
		want     bool
	}{
		{name: "mieru", kernel: "mieru", protocol: "mieru", want: true},
		{name: "juicity", kernel: "juicity", protocol: "juicity", want: true},
		{name: "official naive", kernel: "naive", protocol: "naive", want: true},
		{name: "sudoku", kernel: "sudoku", protocol: "sudoku", want: true},
		{name: "normalizes selection", kernel: " NAIVE ", protocol: " NAIVE ", want: true},
		{name: "rejects naive on mieru", kernel: "mieru", protocol: "naive", want: false},
		{name: "rejects mieru on naive", kernel: "naive", protocol: "mieru", want: false},
		{name: "rejects juicity on naive", kernel: "naive", protocol: "juicity", want: false},
		{name: "rejects sudoku on naive", kernel: "naive", protocol: "sudoku", want: false},
		{name: "rejects removed singbox kernel", kernel: "singbox", protocol: "naive", want: false},
		{name: "rejects unknown kernel", kernel: "unknown", protocol: "naive", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Supports(tt.kernel, tt.protocol); got != tt.want {
				t.Fatalf("Supports(%q, %q) = %v, want %v", tt.kernel, tt.protocol, got, tt.want)
			}
		})
	}
}
