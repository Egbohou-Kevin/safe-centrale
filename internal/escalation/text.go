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
	// maxSilent bounds the sentence on silent sirens: longer, it leaves their names out.
	maxSilent = 64
)

// text builds the message for an alert sent through a channel. Only an SMS says how
// to answer, since the answer is a "1" sent back by SMS: the application and WhatsApp
// carry their own button ("Je gere", "Je m'en occupe"), and a call shows no text.
// An alert of an alarm already handled asks for no answer: it says the alarm is
// handled instead. An alert raised while sirens are known silent says so. These
// sentences are kept whole when the text is cut.
func (e *Escalation) text(in IncidentSnap, ch Channel) string {
	msg, answer := e.message(in)
	silent := e.silentSirens(in.Silent)
	switch {
	case in.Quiet && e.s.Handled[in.Alarm].Closed:
		return fit(msg, silent+" L'alarme a ete arretee.")
	case in.Quiet:
		return fit(msg, silent+" L'alarme est deja prise en charge.")
	case answer && ch == ChannelSMS:
		return fit(msg, silent+ackHint)
	}
	return fit(msg, silent)
}

// message builds what an alert says, and whether it asks to be answered.
func (e *Escalation) message(in IncidentSnap) (msg string, answer bool) {
	site := ascii(e.cfg.SiteName)
	hour := e.hour(in.RaisedAt)
	where := e.deviceList(in.Devices)
	if where != "" {
		where = " (" + where + ")"
	}

	switch in.Kind {
	case KindIntrusion:
		return fmt.Sprintf("ALARME %s: intrusion%s a %s.", site, where, hour), true
	case KindFire:
		return fmt.Sprintf("ALARME INCENDIE %s: fumee detectee%s a %s.", site, where, hour), true
	case KindPanic:
		return fmt.Sprintf("ALARME PANIQUE %s: bouton panique%s a %s.", site, where, hour), true
	case KindTamper:
		return fmt.Sprintf("ALERTE %s: sabotage%s a %s.", site, where, hour), true
	case KindAuthFailed:
		return fmt.Sprintf("ALERTE %s: codes faux repetes a %s.", site, hour), true
	case KindArmingBypassed:
		return fmt.Sprintf("ALERTE %s: alarme armee a %s avec une zone ouverte%s, non protegee.", site, hour, where), true
	case KindHubBatteryLow:
		return fmt.Sprintf("ALERTE %s: centrale sur batterie, %d%% restant (%s).", site, in.Percent, hour), true
	case KindMainsRestored:
		return fmt.Sprintf("%s: le courant est revenu, batterie de la centrale a %d%%.", site, in.Percent), false
	case KindDeviceOffline:
		return fmt.Sprintf("%s: appareils muets%s.", site, where), false
	case KindBatteryLow:
		return fmt.Sprintf("%s: piles faibles%s.", site, where), false
	case KindZigbeeDown:
		return fmt.Sprintf("ALERTE %s: capteurs injoignables depuis %s, l'alarme ne detecte plus rien. Verifiez la centrale.", site, hour), true
	case KindSirenFailed:
		if in.Quiet {
			return fmt.Sprintf("%s: %s n'a pas sonne a %s.", site, e.theSiren(in.Devices), hour), false
		}
		return fmt.Sprintf("ALERTE %s: %s ne repond pas a %s, l'alarme est silencieuse.", site, e.theSiren(in.Devices), hour), true
	case KindGSMDegraded:
		return fmt.Sprintf("%s: protection degradee, SMS indisponible: verifiez la SIM.", site), false
	}
	return fmt.Sprintf("%s: alerte %s a %s.", site, in.Kind, hour), false
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

// sirenWord is how a siren's name says it is one, once in plain ASCII.
const sirenWord = "sirene"

// theSiren names a siren as the subject of a sentence: "la sirene (Reserve)", or "la
// sirene 2" when its name already says it is one (the defaults "Sirene" and "Sirene
// 2", or a name such as "Sirene exterieure"). Several read "la sirene (A, B)".
func (e *Escalation) theSiren(ids []string) string {
	names := e.deviceList(ids)
	switch {
	case names == "":
		return "la " + sirenWord
	case len(ids) == 1 && saysSiren(names):
		return "la " + sirenWord + names[len(sirenWord):]
	}
	return "la " + sirenWord + " (" + names + ")"
}

// saysSiren reports whether a name starts with the word "sirene".
func saysSiren(name string) bool {
	n := len(sirenWord)
	return len(name) >= n && strings.EqualFold(name[:n], sirenWord) && (len(name) == n || name[n] == ' ')
}

// silentSirens is the sentence an alert raised while sirens are known silent ends
// with (Raise.Silent): the people on the site are not warned by them. Their names
// are left out when they would take too much of the text.
func (e *Escalation) silentSirens(ids []string) string {
	var s, short string
	switch len(ids) {
	case 0:
		return ""
	case 1:
		s, short = " "+capital(e.theSiren(ids))+" ne repond pas.", " La sirene ne repond pas."
	default:
		s, short = " Les sirenes ("+e.deviceList(ids)+") ne repondent pas.", " Les sirenes ne repondent pas."
	}
	if len(s) > maxSilent {
		return short
	}
	return s
}

// capital puts a sentence's first letter in capital.
func capital(s string) string { return strings.ToUpper(s[:1]) + s[1:] }

// fit cuts a message to one SMS with its tail (the way to answer) whole: what is cut
// is the message, never the tail.
func fit(msg, tail string) string {
	if len(msg)+len(tail) <= maxText {
		return msg + tail
	}
	room := maxText - len(tail) - 3
	return strings.TrimRight(msg[:room], " ,:(") + "..." + tail
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
