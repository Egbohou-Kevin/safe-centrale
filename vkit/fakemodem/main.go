// Command fakemodem is the GSM modem of the virtual kit (docs/kit-virtuel.md): the AT
// simulator of the tests behind a pseudo-terminal, so that the real safehome-gsm
// opens it as it would a USB modem's serial port. It prints every SMS sent and every
// call made, and takes orders from a named pipe: an incoming SMS, the network lost,
// the modem unplugged.
//
//	fakemodem -link /dev/ttyVMODEM -control /run/vkit/modem
//	echo 'sms +22890000001 STATUT' > /run/vkit/modem
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"syscall"
	"time"

	"safehome/internal/adapter/atmodem/attest"
	"safehome/vkit/order"
)

func main() {
	link := flag.String("link", "/dev/ttyVMODEM", "the serial port the gateway opens (a symbolic link)")
	control := flag.String("control", "/run/vkit/modem", "the named pipe that takes the orders")
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	k := &kit{sim: attest.New(), link: *link, unplug: make(chan struct{}, 1)}
	if err := order.Listen(*control, k.order); err != nil {
		log.Fatal(err)
	}
	go k.report(200 * time.Millisecond)
	for {
		if err := k.session(); err != nil {
			log.Fatal(err)
		}
		log.Print("modem restarted: the port goes away and comes back")
		time.Sleep(time.Second) // a USB modem takes a moment to come back
	}
}

// kit is one simulated modem, plugged into a new pseudo-terminal each time it
// restarts.
type kit struct {
	sim     *attest.Sim
	link    string
	unplug  chan struct{} // an order to end the session, buffered: one at a time
	inboxID int           // guarded by sim.Mu
}

// session plugs the modem into a new pseudo-terminal, and returns when the modem
// goes away (restarted by the gateway, or unplugged by an order). Closing the master
// side hangs up the gateway's port, as a USB modem that disappears does.
func (k *kit) session() error {
	master, slave, err := openPTY()
	if err != nil {
		return err
	}
	if err := relink(slave, k.link); err != nil {
		master.Close()
		return err
	}
	conn, err := k.sim.Open()
	if err != nil {
		master.Close()
		return err
	}
	done := make(chan struct{}, 2)
	go func() { pump(conn, master); done <- struct{}{} }()
	go func() { pump(master, conn); done <- struct{}{} }()
	select {
	case <-done:
	case <-k.unplug:
	}
	master.Close()
	conn.Close()
	<-done
	return nil
}

// pump copies from src to dst until either is closed. A pseudo-terminal answers EIO
// while nobody has its other side open (the gateway between two opens): that is
// waited out, not an end.
func pump(dst io.Writer, src io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil && !errors.Is(werr, syscall.EIO) {
				return
			}
		}
		if errors.Is(err, syscall.EIO) {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if err != nil {
			return
		}
	}
}

// relink points the gateway's port at the new pseudo-terminal, in one step.
func relink(target, link string) error {
	tmp := link + ".new"
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}

// report prints what the gateway did with the modem: the SMS sent and the calls.
func (k *kit) report(every time.Duration) {
	sent, seen := 0, 0
	for range time.Tick(every) {
		k.sim.Mu.Lock()
		for ; sent < len(k.sim.Sent); sent++ {
			fmt.Printf("SMS to=%s text=%q\n", k.sim.Sent[sent][0], k.sim.Sent[sent][1])
		}
		k.sim.Mu.Unlock()
		cmds := k.sim.Commands()
		for ; seen < len(cmds); seen++ {
			if c := cmds[seen]; strings.HasPrefix(c, "ATD") {
				fmt.Printf("CALL to=%s\n", strings.TrimSuffix(c[3:], ";"))
			} else if c == "ATH" {
				fmt.Println("HANGUP")
			}
		}
	}
}
