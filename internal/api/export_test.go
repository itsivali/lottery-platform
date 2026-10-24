package api

import "time"

// SetLiveHeartbeat shortens the stream heartbeat for a test and restores it afterwards.
func SetLiveHeartbeat(d time.Duration) (restore func()) {
	old := liveHeartbeat
	liveHeartbeat = d
	return func() { liveHeartbeat = old }
}
