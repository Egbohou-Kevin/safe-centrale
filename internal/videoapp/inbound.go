package videoapp

import (
	"errors"
	"time"

	"safehome/internal/contract"
	"safehome/internal/inbox"
	"safehome/internal/video"
	videowire "safehome/internal/video/wire"
)

// HandleMessage takes a message from the broker. It returns quickly. Payloads are
// never logged: a request holds the relay's token.
func (a *App) HandleMessage(m inbox.Message) {
	if len(m.Payload) > maxPayload {
		return
	}
	switch m.Topic {
	case contract.AlarmEventTopic(a.cfg.Site):
		a.alarmEvent(m)
	case contract.VideoCmdTopic(a.cfg.Site):
		a.order(m)
	}
}

// order takes the cloud's orders: video.request and video.stop.
func (a *App) order(m inbox.Message) {
	env, err := inbox.Envelope(m, a.cfg.Site, contract.TypeVideoRequest, contract.TypeVideoStop)
	if err != nil {
		a.log.Warn("ignoring an invalid video order", "err", err)
		return
	}
	if !a.seen.FirstTime(env.ID) {
		return
	}
	switch env.Type {
	case contract.TypeVideoRequest:
		d, err := contract.ValidData[contract.VideoRequestData](env)
		if err != nil {
			a.log.Warn("ignoring an invalid video request", "id", env.ID, "err", err)
			return
		}
		a.log.Info("video requested", "request", d) // d masks its token
		a.submit(func(now time.Time) video.Input { return videowire.Request(d, now) })
	case contract.TypeVideoStop:
		d, err := contract.ValidData[contract.VideoStopData](env)
		if err != nil {
			a.log.Warn("ignoring an invalid video stop", "id", env.ID, "err", err)
			return
		}
		a.submit(func(now time.Time) video.Input { return videowire.Stop(d, now) })
	}
}

// alarmEvent takes the alarm's events: only alarm.triggered matters, for photos.
func (a *App) alarmEvent(m inbox.Message) {
	env, d, err := inbox.Open[contract.TriggeredData](m, a.cfg.Site, contract.TypeAlarmTriggered)
	if err == nil && d.Incident == "" {
		err = errors.New("no incident")
	}
	if err != nil {
		if !errors.Is(err, inbox.ErrWrongType) {
			a.log.Warn("ignoring an invalid alarm.triggered", "err", err)
		}
		return
	}
	if a.seen.FirstTime(env.ID) {
		a.submit(func(now time.Time) video.Input { return videowire.Alarm(d, now) })
	}
}

func (a *App) submit(build func(time.Time) video.Input) {
	if err := a.loop.Submit(a.life, build); err != nil && a.life.Err() == nil {
		a.log.Error("handing an input to the video service failed", "err", err)
	}
}
