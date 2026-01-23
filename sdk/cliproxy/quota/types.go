package quota

import "time"

// ModelQuota captures the latest known quota percentage for a model.
type ModelQuota struct {
	Percent   float64
	UpdatedAt time.Time
	ResetTime time.Time
}
