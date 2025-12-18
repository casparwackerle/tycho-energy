package components

import cpuidv2 "github.com/klauspost/cpuid/v2"

// IsAMDCPU returns true if the CPU vendor is AMD (or Hygon, which is AMD-compatible for our purposes).
func IsAMDCPU() bool {
	v := cpuidv2.CPU.VendorID
	return v == cpuidv2.AMD || v == cpuidv2.Hygon
}

// CPUVendorString returns the raw vendor string (e.g. "AuthenticAMD", "GenuineIntel") when available.
func CPUVendorString() string {
	// VendorString is the raw string; if empty, fall back to VendorID string.
	if cpuidv2.CPU.VendorString != "" {
		return cpuidv2.CPU.VendorString
	}
	return cpuidv2.CPU.VendorID.String()
}
