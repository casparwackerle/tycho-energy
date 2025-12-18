package source

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"k8s.io/klog/v2"
)

const (
	hwmonClassDir = "/sys/class/hwmon"
	amdEnergyName = "amd_energy"
)

var (
	energyInputRe = regexp.MustCompile(`^energy([0-9]+)_input$`)
)

// ProbeAmdEnergyHwmon keeps backward compatibility: returns the first match.
func ProbeAmdEnergyHwmon() (path string, ok bool) {
	paths := ProbeAllAmdEnergyHwmon()
	if len(paths) == 0 {
		return "", false
	}
	return paths[0], true
}

// ProbeAllAmdEnergyHwmon returns all hwmonX paths where name == "amd_energy", sorted stably.
// This enables multi-socket support if the driver exposes multiple hwmon instances.
func ProbeAllAmdEnergyHwmon() []string {
	entries, err := os.ReadDir(hwmonClassDir)
	if err != nil {
		klog.V(6).Infof("amd_energy: probe failed to read %s: %v", hwmonClassDir, err)
		return nil
	}

	type found struct {
		path string
		num  int
	}
	var out []found

	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "hwmon") {
			continue
		}

		hwmonPath := filepath.Join(hwmonClassDir, name)
		namePath := filepath.Join(hwmonPath, "name")

		b, err := os.ReadFile(namePath)
		if err != nil {
			continue
		}
		devName := strings.TrimSpace(string(b))
		if devName != amdEnergyName {
			continue
		}

		// Parse hwmon numeric suffix for stable ordering.
		n := -1
		if s := strings.TrimPrefix(name, "hwmon"); s != "" {
			if v, err := strconv.Atoi(s); err == nil {
				n = v
			}
		}

		klog.V(6).Infof("amd_energy: found hwmon device %s (name=%q)", hwmonPath, devName)
		out = append(out, found{path: hwmonPath, num: n})
	}

	if len(out) == 0 {
		klog.V(6).Infof("amd_energy: not found under %s", hwmonClassDir)
		return nil
	}

	sort.Slice(out, func(i, j int) bool {
		// Prefer numeric order if both parsed, else lexical.
		if out[i].num >= 0 && out[j].num >= 0 {
			return out[i].num < out[j].num
		}
		return out[i].path < out[j].path
	})

	res := make([]string, 0, len(out))
	for _, f := range out {
		res = append(res, f.path)
	}
	return res
}

type amdEnergyChannel struct {
	Index int
	Input string // absolute path to energyN_input
	Label string // optional label contents (trimmed)
}

type AmdEnergyHwmonReader struct {
	hwmonDir string

	once     sync.Once
	chans    []amdEnergyChannel
	pkgIdx   int
	coreIdxs []int
	initErr  error

	// Cached file paths to reduce per-poll churn.
	pkgInputPath    string
	coreInputPaths  []string
	dramInputPath   string // optional
	uncoreInputPath string // optional
}

func NewAmdEnergyHwmonReader(hwmonDir string) *AmdEnergyHwmonReader {
	return &AmdEnergyHwmonReader{
		hwmonDir: hwmonDir,
		pkgIdx:   -1,
	}
}

func (r *AmdEnergyHwmonReader) init() {
	if r == nil || r.hwmonDir == "" {
		r.initErr = fmt.Errorf("amd_energy: reader not configured")
		return
	}

	entries, err := os.ReadDir(r.hwmonDir)
	if err != nil {
		r.initErr = fmt.Errorf("amd_energy: readdir %s: %w", r.hwmonDir, err)
		return
	}

	// Discover channels by finding energyN_input files.
	var chans []amdEnergyChannel
	for _, e := range entries {
		name := e.Name()
		m := energyInputRe.FindStringSubmatch(name)
		if len(m) != 2 {
			continue
		}
		idx, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		inputPath := filepath.Join(r.hwmonDir, name)
		labelPath := filepath.Join(r.hwmonDir, fmt.Sprintf("energy%d_label", idx))

		label := ""
		if b, err := os.ReadFile(labelPath); err == nil {
			label = strings.TrimSpace(string(b))
		}

		chans = append(chans, amdEnergyChannel{
			Index: idx,
			Input: inputPath,
			Label: label,
		})
	}

	if len(chans) == 0 {
		r.initErr = fmt.Errorf("amd_energy: no energy*_input files found in %s", r.hwmonDir)
		return
	}

	// Stable ordering by index
	sort.Slice(chans, func(i, j int) bool { return chans[i].Index < chans[j].Index })
	r.chans = chans

	// Select channels + cache paths.
	r.selectChannelsAndCachePaths()

	// Log discovery + selection result
	for _, ch := range r.chans {
		klog.V(6).Infof("amd_energy: hwmon=%s chan idx=%d label=%q input=%s", r.hwmonDir, ch.Index, ch.Label, ch.Input)
	}
	klog.V(6).Infof(
		"amd_energy: hwmon=%s selected pkg=%s cores=%d dram=%s uncore=%s",
		r.hwmonDir,
		r.pkgInputPath,
		len(r.coreInputPaths),
		emptyAsDash(r.dramInputPath),
		emptyAsDash(r.uncoreInputPath),
	)
}

func emptyAsDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (r *AmdEnergyHwmonReader) selectChannelsAndCachePaths() {
	lc := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

	coreSet := map[int]bool{}
	pkgIdx := -1
	dramIdx := -1
	uncoreIdx := -1

	// Pass 1: label-driven selection when labels exist.
	for _, ch := range r.chans {
		l := lc(ch.Label)
		if l == "" {
			continue
		}

		// Core labels (your observed format): Ecore000, Ecore001, ...
		if strings.HasPrefix(l, "ecore") {
			coreSet[ch.Index] = true
			continue
		}

		// Package (future-proof)
		if strings.Contains(l, "package") || l == "pkg" || strings.Contains(l, "pkg") {
			pkgIdx = ch.Index
		}

		// DRAM (future-proof): could be "dram", "mem", "memory"
		if dramIdx < 0 {
			if strings.Contains(l, "dram") || strings.Contains(l, "mem") || strings.Contains(l, "memory") {
				dramIdx = ch.Index
			}
		}

		// Uncore (future-proof): could be "uncore", "pp1", "soc"
		if uncoreIdx < 0 {
			if strings.Contains(l, "uncore") || strings.Contains(l, "pp1") || strings.Contains(l, "soc") {
				uncoreIdx = ch.Index
			}
		}
	}

	// Package fallback: choose highest index that is not a core.
	if pkgIdx < 0 {
		for i := len(r.chans) - 1; i >= 0; i-- {
			idx := r.chans[i].Index
			if !coreSet[idx] {
				// Avoid accidentally selecting dram/uncore if they exist and are not cores:
				// If we detected dram/uncore explicitly, prefer them not to be "package".
				if idx == dramIdx || idx == uncoreIdx {
					continue
				}
				pkgIdx = idx
				break
			}
		}
	}

	// If still not found (very unlikely), allow selecting the highest non-core anyway.
	if pkgIdx < 0 {
		for i := len(r.chans) - 1; i >= 0; i-- {
			idx := r.chans[i].Index
			if !coreSet[idx] {
				pkgIdx = idx
				break
			}
		}
	}

	r.pkgIdx = pkgIdx

	// Cache core paths in ascending order.
	var corePaths []string
	for _, ch := range r.chans {
		if coreSet[ch.Index] {
			r.coreIdxs = append(r.coreIdxs, ch.Index)
			corePaths = append(corePaths, ch.Input) // already absolute
		}
	}
	r.coreInputPaths = corePaths

	// Cache pkg/dram/uncore paths.
	r.pkgInputPath = findInputByIdx(r.chans, r.pkgIdx)
	r.dramInputPath = findInputByIdx(r.chans, dramIdx)
	r.uncoreInputPath = findInputByIdx(r.chans, uncoreIdx)
}

func findInputByIdx(chans []amdEnergyChannel, idx int) string {
	if idx < 0 {
		return ""
	}
	for _, ch := range chans {
		if ch.Index == idx {
			return ch.Input
		}
	}
	return ""
}

func (r *AmdEnergyHwmonReader) ReadAmdEnergyPackageUJ() (uint64, error) {
	if r == nil {
		return 0, fmt.Errorf("amd_energy: nil reader")
	}
	r.once.Do(r.init)
	if r.initErr != nil {
		return 0, r.initErr
	}
	if r.pkgInputPath == "" {
		return 0, fmt.Errorf("amd_energy: package channel not selected")
	}
	return readUintFromFile(r.pkgInputPath)
}

func (r *AmdEnergyHwmonReader) ReadAmdEnergyCoreTotalUJ() (uint64, error) {
	if r == nil {
		return 0, fmt.Errorf("amd_energy: nil reader")
	}
	r.once.Do(r.init)
	if r.initErr != nil {
		return 0, r.initErr
	}

	var sum uint64
	for _, p := range r.coreInputPaths {
		v, err := readUintFromFile(p)
		if err != nil {
			klog.V(6).Infof("amd_energy: hwmon=%s read core %s failed: %v", r.hwmonDir, p, err)
			continue
		}
		sum += v
	}
	return sum, nil
}

// Optional: if not present, returns 0,nil.
func (r *AmdEnergyHwmonReader) ReadAmdEnergyDramUJ() (uint64, error) {
	if r == nil {
		return 0, fmt.Errorf("amd_energy: nil reader")
	}
	r.once.Do(r.init)
	if r.initErr != nil {
		return 0, r.initErr
	}
	if r.dramInputPath == "" {
		return 0, nil
	}
	return readUintFromFile(r.dramInputPath)
}

// Optional: if not present, returns 0,nil.
func (r *AmdEnergyHwmonReader) ReadAmdEnergyUncoreUJ() (uint64, error) {
	if r == nil {
		return 0, fmt.Errorf("amd_energy: nil reader")
	}
	r.once.Do(r.init)
	if r.initErr != nil {
		return 0, r.initErr
	}
	if r.uncoreInputPath == "" {
		return 0, nil
	}
	return readUintFromFile(r.uncoreInputPath)
}

func readUintFromFile(p string) (uint64, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// PowerAmdEnergyHwmon implements a powerInterface-compatible source for AMD systems.
// Supports multiple hwmon devices (best-effort multi-socket).
type PowerAmdEnergyHwmon struct {
	hwmonDirs []string
	readers   []*AmdEnergyHwmonReader
}

func NewPowerAmdEnergyHwmonFromDirs(hwmonDirs []string) *PowerAmdEnergyHwmon {
	p := &PowerAmdEnergyHwmon{hwmonDirs: append([]string{}, hwmonDirs...)}
	for _, d := range p.hwmonDirs {
		p.readers = append(p.readers, NewAmdEnergyHwmonReader(d))
	}
	return p
}

func (p *PowerAmdEnergyHwmon) GetName() string { return "amd-energy-hwmon" }

func (p *PowerAmdEnergyHwmon) IsSystemCollectionSupported() bool {
	if p == nil || len(p.hwmonDirs) == 0 {
		return false
	}
	for _, d := range p.hwmonDirs {
		b, err := os.ReadFile(filepath.Join(d, "name"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(b)) == amdEnergyName {
			return true
		}
	}
	return false
}

func (p *PowerAmdEnergyHwmon) StopPower() {}

func (p *PowerAmdEnergyHwmon) GetAbsEnergyFromPackage() (uint64, error) {
	if p == nil {
		return 0, fmt.Errorf("amd_energy: not initialized")
	}
	var sumUJ uint64
	var any bool
	for _, r := range p.readers {
		uJ, err := r.ReadAmdEnergyPackageUJ()
		if err != nil {
			klog.V(6).Infof("amd_energy: pkg read failed: %v", err)
			continue
		}
		sumUJ += uJ
		any = true
	}
	if !any {
		return 0, fmt.Errorf("amd_energy: package unavailable")
	}
	return sumUJ / 1000, nil // mJ
}

func (p *PowerAmdEnergyHwmon) GetAbsEnergyFromCore() (uint64, error) {
	if p == nil {
		return 0, fmt.Errorf("amd_energy: not initialized")
	}
	var sumUJ uint64
	var any bool
	for _, r := range p.readers {
		uJ, err := r.ReadAmdEnergyCoreTotalUJ()
		if err != nil {
			klog.V(6).Infof("amd_energy: core read failed: %v", err)
			continue
		}
		sumUJ += uJ
		any = true
	}
	if !any {
		return 0, fmt.Errorf("amd_energy: core unavailable")
	}
	return sumUJ / 1000, nil // mJ
}

// Single-value getters return the sum across sockets, consistent with sysfs behavior.
func (p *PowerAmdEnergyHwmon) GetAbsEnergyFromDram() (uint64, error) {
	if p == nil {
		return 0, fmt.Errorf("amd_energy: not initialized")
	}
	var sumUJ uint64
	for _, r := range p.readers {
		uJ, err := r.ReadAmdEnergyDramUJ()
		if err != nil {
			klog.V(6).Infof("amd_energy: dram read failed: %v", err)
			continue
		}
		sumUJ += uJ
	}
	return sumUJ / 1000, nil
}

func (p *PowerAmdEnergyHwmon) GetAbsEnergyFromUncore() (uint64, error) {
	if p == nil {
		return 0, fmt.Errorf("amd_energy: not initialized")
	}
	var sumUJ uint64
	for _, r := range p.readers {
		uJ, err := r.ReadAmdEnergyUncoreUJ()
		if err != nil {
			klog.V(6).Infof("amd_energy: uncore read failed: %v", err)
			continue
		}
		sumUJ += uJ
	}
	return sumUJ / 1000, nil
}

func (p *PowerAmdEnergyHwmon) GetAbsEnergyFromNodeComponents() map[int]NodeComponentsEnergy {
	out := make(map[int]NodeComponentsEnergy)

	for i, r := range p.readers {
		pkgUJ, err := r.ReadAmdEnergyPackageUJ()
		if err != nil {
			klog.V(6).Infof("amd_energy: socket=%d pkg read failed: %v", i, err)
			continue
		}
		coreUJ, err := r.ReadAmdEnergyCoreTotalUJ()
		if err != nil {
			klog.V(6).Infof("amd_energy: socket=%d core read failed: %v", i, err)
			coreUJ = 0
		}

		// Optional domains: return 0 if absent.
		dramUJ, err := r.ReadAmdEnergyDramUJ()
		if err != nil {
			klog.V(6).Infof("amd_energy: socket=%d dram read failed: %v", i, err)
			dramUJ = 0
		}
		uncoreUJ, err := r.ReadAmdEnergyUncoreUJ()
		if err != nil {
			klog.V(6).Infof("amd_energy: socket=%d uncore read failed: %v", i, err)
			uncoreUJ = 0
		}

		out[i] = NodeComponentsEnergy{
			Pkg:    pkgUJ / 1000,
			Core:   coreUJ / 1000,
			Uncore: uncoreUJ / 1000,
			DRAM:   dramUJ / 1000,
		}
	}

	return out
}
