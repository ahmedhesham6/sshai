package domain

import "time"

type User struct {
	ID            string
	WorkOSUserID  string
	DefaultRegion string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
