// Command pspsim runs a simulated sponsor PSP bank (with the NPCI network and payer side behind it)
// for local development, demos and integration tests.
package main

import (
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"
	_ "time/tzdata"

	"github.com/AppTestify/UPI-Switch/internal/config"
	"github.com/AppTestify/UPI-Switch/internal/pspsim"
	"github.com/AppTestify/UPI-Switch/internal/security"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	must := func(err error) {
		if err != nil {
			log.Error("pspsim", "err", err)
			os.Exit(1)
		}
	}
	key, err := security.LoadPrivateKey(config.Get("SIM_KEY", "keys/bank-a.key"))
	must(err)
	switchKey, err := security.LoadPublicKey(config.Get("SWITCH_PUBLIC_KEY", "keys/switch.pub"))
	must(err)
	loc, err := time.LoadLocation(config.Get("SIM_TZ", "Asia/Kolkata"))
	must(err)
	delay, err := time.ParseDuration(config.Get("CALLBACK_DELAY", "500ms"))
	must(err)
	tech, _ := strconv.ParseFloat(config.Get("TECH_DECLINE_RATE", "0"), 64)
	drop, _ := strconv.ParseFloat(config.Get("DROP_CALLBACKS", "0"), 64)
	id := config.Get("SIM_ID", "bank-a")
	sim := pspsim.New(pspsim.Config{
		ID: id, OrgID: config.Get("SIM_ORG", "BKA"), Handle: config.Get("SIM_HANDLE", "banka"),
		CallbackURL:   config.Get("SWITCH_CALLBACK_URL", "http://localhost:8080/v1/npci/callback/"+id),
		CallbackDelay: delay, TechDeclineRate: tech, DropCallbacks: drop, Location: loc,
	}, security.NewSoftHSM(key, id), switchKey, log)
	addr := config.Get("LISTEN_ADDR", ":9101")
	log.Info("psp simulator listening", "addr", addr, "psp", id)
	srv := &http.Server{Addr: addr, Handler: sim.Handler(), ReadHeaderTimeout: 5 * time.Second}
	must(srv.ListenAndServe())
}
