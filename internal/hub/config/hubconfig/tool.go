package hubconfig

import (
	"encoding/json"
	"fmt"
	"io"

	"safehome/internal/shared/config/configfile"
	"safehome/internal/shared/config/hubtool"
)

// maxSiteInput bounds the site a tool sends: a few dozen zones fit in some kilobytes.
const maxSiteInput = 1 << 20

// SiteTool answers the installer's tools about the hub whose configuration is at
// path (safehome-hub site -config FILE VERB, internal/shared/config/hubtool): the
// tools never read the hub's files, the hub checks by its own rules. It returns the
// exit code; a refusal is said on stderr.
func SiteTool(path string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(stderr, "%s: say %s, %s, %s or %s\n", hubtool.Command, hubtool.View, hubtool.Read, hubtool.Check, hubtool.Write)
		return 2
	}
	if err := siteTool(path, args[0], stdin, stdout); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func siteTool(path, verb string, stdin io.Reader, stdout io.Writer) error {
	switch verb {
	case hubtool.View:
		r, err := Load(path)
		if err != nil {
			return err
		}
		return printJSON(stdout, ViewOf(r))
	case hubtool.Read:
		s, err := ReadSite(path)
		if err != nil {
			return err
		}
		return printJSON(stdout, hubtool.SiteOf(s))
	case hubtool.Check, hubtool.Write:
		data, err := io.ReadAll(io.LimitReader(stdin, maxSiteInput+1))
		if err != nil {
			return fmt.Errorf("hubconfig: the site: %w", err)
		}
		if len(data) > maxSiteInput {
			return fmt.Errorf("hubconfig: the site is over %d bytes", maxSiteInput)
		}
		var s hubtool.Site
		if err := configfile.DecodeBytes(data, &s); err != nil {
			return fmt.Errorf("hubconfig: the site: %w", err)
		}
		if verb == hubtool.Check {
			return CheckSite(path, s.Model())
		}
		return WriteSite(path, s.Model())
	}
	return fmt.Errorf("%s: unknown %q", hubtool.Command, verb)
}

// ViewOf is what the installer's tools know of the configuration r.
func ViewOf(r Result) hubtool.Config {
	v := hubtool.Config{Site: r.Hub.Site, StateDir: r.StateDir, BrokerURL: r.BrokerURL,
		Contacts: []hubtool.Contact{}, Access: hubtool.AccessOf(r.Hub.Access)}
	if loc := r.Hub.Engine.Location; loc != nil {
		v.Timezone = loc.String()
	}
	for _, c := range r.Hub.Escalation.Contacts {
		v.Contacts = append(v.Contacts, hubtool.Contact{ID: c.ID, Name: c.Name, Role: string(c.Role)})
	}
	return v
}

func printJSON(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("hubconfig: %w", err)
	}
	_, err = w.Write(append(data, '\n'))
	return err
}
