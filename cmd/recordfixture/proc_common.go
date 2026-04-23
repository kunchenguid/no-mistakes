package main

import "time"

func terminateWithFallback(terminate func() error, kill func() error, done <-chan error, grace time.Duration, ignorable func(error) bool) error {
	if err := terminate(); err != nil {
		if !ignorable(err) {
			return err
		}
	} else {
		select {
		case err := <-done:
			return err
		case <-time.After(grace):
		}
	}
	if err := kill(); err != nil {
		return err
	}
	return <-done
}
