package videoapp

import (
	"time"

	"safehome/internal/contract"
	"safehome/internal/video"
	videowire "safehome/internal/video/wire"
)

// HandleMessage takes a message from the broker. It returns quickly. Payloads are
// never logged: a request holds the relay's token.
func (a *App) HandleMessage(m Message) {
	if len(m.Payload) > maxPayload {
		return
	}
	switch m.Topic {
	case contract.AlarmEventTopic(a.cfg.Site):
		a.alarmEvent(m.Payload)
	case contract.VideoCmdTopic(a.cfg.Site):
		a.order(m.Payload)
	}
}

// order takes the cloud's orders: video.request and video.stop.
func (a *App) order(payload []byte) {
	env, err := contract.ParseEnvelope(payload)
	if err != nil || env.Site != a.cfg.Site {
		a.log.Warn("ignoring an invalid video order", "err", err)
		return
	}
	if !a.seen.FirstTime(env.ID) {
		return
	}
	switch env.Type {
	case contract.TypeVideoRequest:
		d, err := decodeOrder[contract.VideoRequestData](env)
		if err != nil {
			a.log.Warn("ignoring an invalid video request", "id", env.ID, "err", err)
			return
		}
		a.log.Info("video requested", "request", d) // d masks its token
		a.submit(func(now time.Time) video.Input { return videowire.Request(d, now) })
	case contract.TypeVideoStop:
		d, err := decodeOrder[contract.VideoStopData](env)
		if err != nil {
			a.log.Warn("ignoring an invalid video stop", "id", env.ID, "err", err)
			return
		}
		a.submit(func(now time.Time) video.Input { return videowire.Stop(d, now) })
	default:
		a.log.Warn("ignoring an unknown video order", "type", env.Type)
	}
}

// decodeOrder decodes an order's data and checks it.
func decodeOrder[T interface{ Validate() error }](env contract.Envelope) (T, error) {
	d, err := contract.DecodeData[T](env)
	if err == nil {
		err = d.Validate()
	}
	return d, err
}

// alarmEvent takes the alarm's events: only alarm.triggered matters, for photos.
func (a *App) alarmEvent(payload []byte) {
	env, err := contract.ParseEnvelope(payload)
	if err != nil || env.Site != a.cfg.Site || env.Type != contract.TypeAlarmTriggered || !a.seen.FirstTime(env.ID) {
		return
	}
	d, err := contract.DecodeData[contract.TriggeredData](env)
	if err != nil || d.Incident == "" {
		a.log.Warn("ignoring an invalid alarm.triggered", "err", err)
		return
	}
	a.submit(func(now time.Time) video.Input { return videowire.Alarm(d, now) })
}

func (a *App) submit(build func(time.Time) video.Input) {
	if err := a.loop.Submit(a.life, build); err != nil && a.life.Err() == nil {
		a.log.Error("handing an input to the video service failed", "err", err)
	}
}
