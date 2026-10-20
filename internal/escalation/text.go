package escalation

import (
	"fmt"
	"strings"
	"time"
)

// The texts are product text for the contacts of a site in Togo, so they are in
// French; they are plain ASCII and fit one SMS of 160 characters.
const (
	maxText = 160
	ackHint = " Repondre 1 si vous gerez."
)

// text builds the message for an alert.
func (e *Escalation) text(in IncidentSnap) string {
	site := ascii(e.cfg.SiteName)
	hour := e.hour(in.RaisedAt)
	where := e.deviceList(in.Devices)
	if where != "" {
		where = " (" + where + ")"
	}

	var msg string
	switch in.Kind {
	case KindIntrusion:
		msg = fmt.Sprintf("ALARME %s: intrusion%s a %s.%s", site, where, hour, ackHint)
	case KindFire:
		msg = fmt.Sprintf("ALARME INCENDIE %s: fumee detectee%s a %s.%s", site, where, hour, ackHint)
	case KindPanic:
		msg = fmt.Sprintf("ALARME PANIQUE %s: bouton panique%s a %s.%s", site, where, hour, ackHint)
	case KindTamper:
		msg = fmt.Sprintf("ALERTE %s: sabotage%s a %s.%s", site, where, hour, ackHint)
	case KindAuthFailed:
		msg = fmt.Sprintf("ALERTE %s: codes faux repetes a %s.%s", site, hour, ackHint)
	case KindArmingBypassed:
		msg = fmt.Sprintf("ALERTE %s: armement automatique a %s avec une zone ouverte%s, non protegee.%s", site, hour, where, ackHint)
	case KindHubBatteryLow:
		msg = fmt.Sprintf("ALERTE %s: centrale sur batterie, %d%% restant (%s).%s", site, in.Percent, hour, ackHint)
	case KindMainsRestored:
		msg = fmt.Sprintf("%s: le courant est revenu, batterie de la centrale a %d%%.", site, in.Percent)
	case KindDeviceOffline:
		msg = fmt.Sprintf("%s: appareils muets%s.", site, where)
	case KindBatteryLow:
		msg = fmt.Sprintf("%s: piles faibles%s.", site, where)
	case KindZigbeeDown:
		msg = fmt.Sprintf("ALERTE %s: capteurs injoignables depuis %s, l'alarme ne detecte plus rien. Verifiez la centrale.%s", site, hour, ackHint)
	case KindGSMDegraded:
		msg = fmt.Sprintf("%s: protection degradee, SMS indisponible: verifiez la SIM.", site)
	default:
		msg = fmt.Sprintf("%s: alerte %s a %s.", site, in.Kind, hour)
	}
	return fit(msg)
}

func (e *Escalation) hour(t time.Time) string {
	loc := e.cfg.Location
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("15:04")
}

// deviceList names the devices, by their configured name when there is one.
func (e *Escalation) deviceList(ids []string) string {
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		if n, ok := e.cfg.DeviceNames[id]; ok && n != "" {
			id = n
		}
		names = append(names, ascii(id))
	}
	return strings.Join(names, ", ")
}

// fit cuts a message to one SMS, keeping its end (the way to answer) when it can.
func fit(msg string) string {
	if len(msg) <= maxText {
		return msg
	}
	if strings.HasSuffix(msg, ackHint) {
		head := msg[:len(msg)-len(ackHint)]
		room := maxText - len(ackHint) - 3
		if room > 0 {
			return strings.TrimRight(head[:min(room, len(head))], " ,:(") + "..." + ackHint
		}
	}
	return msg[:maxText-3] + "..."
}

var accents = strings.NewReplacer(
	"\u00e0", "a", "\u00e2", "a", "\u00e4", "a", "\u00e1", "a", "\u00e9", "e", "\u00e8", "e", "\u00ea", "e", "\u00eb", "e",
	"\u00ee", "i", "\u00ef", "i", "\u00ed", "i", "\u00f4", "o", "\u00f6", "o", "\u00f3", "o", "\u00f9", "u", "\u00fb", "u", "\u00fc", "u",
	"\u00fa", "u", "\u00e7", "c", "\u00f1", "n", "\u0153", "oe", "\u00e6", "ae",
	"\u00c0", "A", "\u00c2", "A", "\u00c4", "A", "\u00c9", "E", "\u00c8", "E", "\u00ca", "E", "\u00cb", "E", "\u00ce", "I", "\u00cf", "I",
	"\u00d4", "O", "\u00d6", "O", "\u00d9", "U", "\u00db", "U", "\u00dc", "U", "\u00c7", "C", "\u2019", "'", "\u2018", "'",
)

// ascii makes a name safe for an SMS: accents are removed, anything else that is not
// plain ASCII is dropped.
func ascii(s string) string {
	s = accents.Replace(s)
	return strings.Map(func(r rune) rune {
		if r >= 0x20 && r < 0x7f {
			return r
		}
		return -1
	}, s)
}
