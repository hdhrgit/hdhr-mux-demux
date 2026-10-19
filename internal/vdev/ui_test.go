package vdev

import "testing"

func TestGuidePlaceholders(t *testing.T) {
	for _, title := range []string{"To Be Announced", "to be announced", "TBA", "TBD", "Program information is currently unavailable.", "Programming Information Not Available", "  To Be Announced "} {
		if !rePlaceholder.MatchString(title) {
			t.Errorf("%q should be a placeholder", title)
		}
	}
	for _, title := range []string{"The Love Boat", "Tbilisi Stories", "Announced Live", "To Be Continued", "TBS Late Night"} {
		if rePlaceholder.MatchString(title) {
			t.Errorf("%q must not be a placeholder", title)
		}
	}
}
