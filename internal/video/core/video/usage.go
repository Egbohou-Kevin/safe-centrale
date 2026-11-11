package video

// month starts the usage again from zero in a later month. An earlier month does not:
// a clock set back (a dead clock module at boot, corrected later by the network) must
// not wipe the usage the client already paid for this month, twice. Only a clock that
// NTP vouches for (a Tick with Synced) takes the month back: the saved one then came
// from a clock that ran ahead, which would hold the usage of the real months in it
// until that month comes, years later maybe, and the allowance used up there would
// refuse every session meanwhile. The real month gets back its usage from before the
// clock jumped (Prior), and keeps what was used since.
func (v *Service) month(in Input) {
	month := in.at().UTC().Format("2006-01")
	tick, _ := in.(Tick)
	switch {
	case month > v.s.Month:
		v.keepUsed()
		v.s.Prior, v.s.PriorMonth = v.s.Used, v.s.Month
		v.s.Month, v.s.Used = month, 0
	case month < v.s.Month && tick.Synced:
		v.keepUsed()
		if month == v.s.PriorMonth {
			v.s.Used += v.s.Prior
		}
		v.s.Month, v.s.Prior, v.s.PriorMonth = month, 0, ""
	}
}

func (v *Service) used() int64 { return v.s.Used + v.unsaved }

// keepUsed puts the bytes counted since the last save into the saved usage.
func (v *Service) keepUsed() { v.s.Used, v.unsaved = v.used(), 0 }
