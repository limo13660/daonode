package core

import (
	"reflect"
	"testing"
)

func TestKernelCapabilitiesContract(t *testing.T) {
	want := []KernelCapability{
		{Name: "mieru", Protocols: []string{"mieru"}},
		{Name: "singbox", Protocols: []string{"naive"}},
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
		{name: "singbox naive", kernel: "singbox", protocol: "naive", want: true},
		{name: "normalizes selection", kernel: " SINGBOX ", protocol: " NAIVE ", want: true},
		{name: "rejects naive on mieru", kernel: "mieru", protocol: "naive", want: false},
		{name: "rejects mieru on singbox", kernel: "singbox", protocol: "mieru", want: false},
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
