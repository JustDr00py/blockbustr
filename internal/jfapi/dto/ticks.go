package dto

import "time"

// Jellyfin durations and positions (RunTimeTicks, PositionTicks, …) are .NET
// TimeSpan ticks: 100 ns units.
const TicksPerSecond int64 = 10_000_000

// TicksFromDuration converts d to ticks, truncating below 100 ns.
func TicksFromDuration(d time.Duration) int64 { return int64(d / 100) }

// DurationFromTicks converts ticks to a time.Duration.
func DurationFromTicks(ticks int64) time.Duration { return time.Duration(ticks) * 100 }
