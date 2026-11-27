package main

import (
	"flag"
	"log"
	"time"
)

func main() {
	fixtureDir := flag.String("fixtures", "", "download and normalize real photos into this directory")
	base := flag.String("base", "http://127.0.0.1:8082", "isolated load test URL")
	users := flag.Int("users", 500, "number of simulated viewers")
	arrival := flag.Duration("arrival", 12*time.Second, "window for opening the page")
	burst := flag.Duration("burst", 5*time.Second, "window for submitting ballots")
	race := flag.Bool("close-race", false, "close during a burst of new submissions")
	ballotsOnly := flag.Bool("ballots-only", false, "prewarm browser sessions and submit a concentrated ballot burst")
	flag.Parse()
	var err error
	switch {
	case *fixtureDir != "":
		err = prepareFixtures(*fixtureDir)
	case *users < 1 || *users > 1000 || *arrival < 0 || *burst < 0:
		log.Fatal("invalid user count or window")
	case *race:
		err = runCloseRace(*base, *users)
	case *ballotsOnly:
		err = runBallotBurst(*base, *users, *burst)
	default:
		err = runProfile(*base, *users, *arrival, *burst)
	}
	if err != nil {
		log.Fatal(err)
	}
}
