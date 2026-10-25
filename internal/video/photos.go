package video

import (
	"fmt"
	"slices"
	"time"
)

// photoLate is how old an alarm may be when it reaches the service and still get its
// photos: the broker keeps the alarms for the service while it is down (a crash loop,
// a configuration to fix), and hands them over all at once when it comes back, maybe
// hours later. Photos taken then would show the shop as it is now, staff and
// customers in frame, filed as the intrusion's; and they would use up the hour's
// allowance that a live alarm needs.
const photoLate = 5 * time.Minute

// alarm decides the photos of an alarm: once per incident, PerHour alarms an hour at
// most, of the first cameras, none for an alarm older than photoLate (it does not
// count in the hour). They are taken even past the month's allowance: a few hundred
// kilobytes that may show who broke in.
func (v *Service) alarm(a Alarm) []Action {
	p := v.cfg.Photos
	if p.Count == 0 || a.Incident == "" || slices.Contains(v.s.Photographed, a.Incident) {
		return nil
	}
	v.s.Photographed = append(v.s.Photographed, a.Incident)
	if over := len(v.s.Photographed) - photoMemory; over > 0 {
		v.s.Photographed = slices.Delete(v.s.Photographed, 0, over)
	}
	if age := a.Now.Sub(a.At); !a.At.IsZero() && age > photoLate {
		return []Action{NoPhotos{Incident: a.Incident, Why: fmt.Sprintf("the alarm is %s old: photos now would not show it", age.Round(time.Second))}}
	}
	for i, t := range v.s.PhotosAt {
		if t.After(a.Now) {
			v.s.PhotosAt[i] = a.Now // saved while the clock was ahead: not an hour of refusals more
		}
	}
	v.s.PhotosAt = slices.DeleteFunc(v.s.PhotosAt, func(t time.Time) bool { return !a.Now.Before(t.Add(time.Hour)) })
	if len(v.s.PhotosAt) >= p.PerHour {
		return []Action{NoPhotos{Incident: a.Incident, Why: fmt.Sprintf("already %d alarms photographed this hour", p.PerHour)}}
	}
	v.s.PhotosAt = append(v.s.PhotosAt, a.Now)
	var cams []string
	for _, c := range v.cfg.Cameras[:min(len(v.cfg.Cameras), MaxPhotoCameras)] {
		cams = append(cams, c.ID)
	}
	return []Action{TakePhotos{Incident: a.Incident, Cameras: cams, Count: p.Count, Every: p.Every, Width: p.Width}}
}
