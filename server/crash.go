package server

import (
	"fmt"
	"github.com/pkg/errors"
	"github.com/pterodactyl/wings/config"
	"time"
)

type CrashDetection struct {
	// If set to false, the system will not listen for crash detection events that
	// can indicate that the server stopped unexpectedly.
	Enabled bool `default:"true" json:"enabled" yaml:"enabled"`

	// Tracks the time of the last server crash event.
	lastCrash time.Time
}

// Looks at the environment exit state to determine if the process exited cleanly or
// if it was the result of an event that we should try to recover from.
//
// This function assumes it is called under circumstances where a crash is suspected
// of occuring. It will not do anything to determine if it was actually a crash, just
// look at the exit state and check if it meets the criteria of being called a crash
// by Wings.
//
// If the server is determined to have crashed, the process will be restarted and the
// counter for the server will be incremented.
func (s *Server) handleServerCrash() error {
	// No point in doing anything here if the server isn't currently offline, there
	// is no reason to do a crash detection event. If the server crash detection is
	// disabled we want to skip anything after this as well.
	if s.GetState() != ProcessOfflineState || !s.CrashDetection.Enabled {
		if !s.CrashDetection.Enabled {
			s.Log().Debug("server triggered crash detection but handler is disabled for server process")

			s.PublishConsoleOutputFromDaemon("Server detected as crashed; crash detection is disabled for this instance.")
		}

		return nil
	}

	exitCode, oomKilled, err := s.Environment.ExitState()
	if err != nil {
		return errors.WithStack(err)
	}

	// If the system is not configured to detect a clean exit code as a crash, and the
	// crash is not the result of the program running out of memory, do nothing.
	if exitCode == 0 && !oomKilled && !config.Get().System.DetectCleanExitAsCrash {
		s.Log().Debug("server exited with successful exit code; system is configured to not detect this as a crash")

		return nil
	}

	s.PublishConsoleOutputFromDaemon("---------- Detected server process in a crashed state! ----------")
	s.PublishConsoleOutputFromDaemon(fmt.Sprintf("Exit code: %d", exitCode))
	s.PublishConsoleOutputFromDaemon(fmt.Sprintf("Out of memory: %t", oomKilled))

	c := s.CrashDetection.lastCrash
	// If the last crash time was within the last 60 seconds we do not want to perform
	// an automatic reboot of the process. Return an error that can be handled.
	if !c.IsZero() && c.Add(time.Second * 60).After(time.Now()) {
		s.PublishConsoleOutputFromDaemon("Aborting automatic reboot: last crash occurred less than 60 seconds ago.")

		return &crashTooFrequent{}
	}

	s.CrashDetection.lastCrash = time.Now()

	return s.Environment.Start()
}