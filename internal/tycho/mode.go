package tycho

import "fmt"

type Mode int

const (
	ModeOff Mode = iota
	ModeSkeleton
)

func (m Mode) String() string {
	switch m {
	case ModeOff:
		return "off"
	case ModeSkeleton:
		return "skeleton"
	default:
		return fmt.Sprintf("unknown(%d)", int(m))
	}
}

func ParseMode(s string) (Mode, error) {
	switch s {
	case "", "off":
		return ModeOff, nil
	case "skeleton":
		return ModeSkeleton, nil
	default:
		return ModeOff, fmt.Errorf("invalid tycho mode %q (allowed: off|skeleton)", s)
	}
}
