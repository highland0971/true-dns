//go:build !windows && !linux

package platform

import (
	"fmt"
	"runtime"
)

func init() { current = stubPlatform{} }

type stubPlatform struct{}

func (stubPlatform) Name() string { return runtime.GOOS }

func (stubPlatform) SetSystemDNS() (*TakeoverState, error) {
	return nil, fmt.Errorf("system DNS takeover is not supported on %s", runtime.GOOS)
}

func (stubPlatform) RestoreSystemDNS(*TakeoverState) error {
	return fmt.Errorf("system DNS takeover is not supported on %s", runtime.GOOS)
}

func (stubPlatform) FlushDNSCache() error { return nil }

func (stubPlatform) DiscoverSystemDNS() []string {
	return normalizeAddrs(DefaultFallbackServers)
}

func (stubPlatform) DescribeState(state *TakeoverState) string {
	return fmt.Sprintf("%s takeover taken at %s", runtime.GOOS, state.TakenAt.Format("2006-01-02 15:04:05"))
}
