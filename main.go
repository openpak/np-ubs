// Command np-ubs is the PS3 firmware's Online Storage for save data (host family "ubs":
// /ubs/sec/set_save_data, get_save_data, get_save_data_info_list, delete_save_data,
// is_supported_title, notify_downloaded) plus its signed discovery file ubstorage.xml.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"openpak/np-common/coreclient"
	"openpak/np-common/nphttp"
	"openpak/np-common/ticket"
	"os"
	"os/signal"
	"strconv"
	"syscall"
)

func main() {
	if err := run(); err != nil {
		slog.Error("np-ubs stopped", "err", err)
		os.Exit(1)
	}
}

func envBytes(key string, def int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive byte count", key)
	}
	return n, nil
}

func run() error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	savesURL, savesKey := os.Getenv("NP_SAVES_URL"), os.Getenv("NP_SAVES_KEY")
	if savesURL == "" || savesKey == "" {
		return fmt.Errorf("NP_SAVES_URL and NP_SAVES_KEY must be set (the core saves service)")
	}
	if len(os.Getenv("NP_CORE_KEY")) < 32 {
		return fmt.Errorf("NP_CORE_KEY must be at least 32 bytes")
	}
	resourceKey, err := os.ReadFile(os.Getenv("NP_UBS_RESOURCE_KEY"))
	if err != nil || len(resourceKey) != 20 {
		return fmt.Errorf("NP_UBS_RESOURCE_KEY must name a 20-byte key file")
	}
	saveMax, err := envBytes("NP_UBS_SAVE_MAX", 1<<30)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	core, err := coreclient.Dial(nphttp.Env("NP_CORE_ADDR", "localhost:7000"), os.Getenv("NP_CORE_KEY"))
	if err != nil {
		return err
	}
	defer core.Close()
	signer, err := ticket.Load(os.Getenv("NP_TICKET_KEY"))
	if err != nil {
		return err
	}
	fqdn := nphttp.Env("NP_UBS_FQDN", "ubs.np.community.playstation.net")
	svc := &Service{Core: core, Tickets: signer, Store: NewSavesClient(savesURL, savesKey), ResourceKey: resourceKey, FQDN: fqdn,
		IconBase: nphttp.Env("NP_UBS_ICON_BASE", "http://"+fqdn), SaveMax: saveMax, Log: log}
	mux := http.NewServeMux()
	svc.Register(mux)
	nphttp.Health(mux)
	front, err := nphttp.Front()
	if err != nil {
		return err
	}
	return nphttp.Run(ctx, mux, nphttp.Env("NP_HTTP_ADDR", ":8536"), os.Getenv("NP_TLS_ADDR"), front)
}
