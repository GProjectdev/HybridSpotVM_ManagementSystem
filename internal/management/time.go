package management

import "time"

func defaultClock() time.Time {
	return time.Now().UTC()
}
