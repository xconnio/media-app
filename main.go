// media-app serves deskconn's Pictures, Videos and Documents apps: it indexes the user's
// standard folders and answers deskconnd's index queries over deskconnd's local socket
// (deskconn.sock). It registers those apps with deskconnd, so they're listed for the
// frontend while media-app is connected.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/deskconn/common"
	"github.com/xconnio/xconn-go"
)

// app is an app media-app serves, as registered with deskconnd.
type app struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

//nolint:gochecknoglobals // fixed list
var apps = []app{{"pictures", "Pictures"}, {"videos", "Videos"}, {"documents", "Documents"}}

const reconnectDelay = 2 * time.Second

func main() {
	cfgDirectory, err := common.CfgDirectory()
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	index, err := NewIndexService(cfgDirectory)
	if err != nil {
		log.Fatalf("media-app: failed to start indexing: %v", err)
	}
	defer index.Close()
	index.Start(ctx)

	// deskconnd may not be up yet, or may restart: keep reconnecting until stopped.
	uri := "unix://" + filepath.Join(cfgDirectory, "deskconn.sock")
	for ctx.Err() == nil {
		if err := serve(ctx, uri, index); err != nil {
			log.Printf("media-app: %v, retrying in %v", err, reconnectDelay)
		}
		select {
		case <-ctx.Done():
		case <-time.After(reconnectDelay):
		}
	}
}

// serve connects to deskconnd and serves it until ctx is done or the connection drops.
func serve(ctx context.Context, uri string, index *IndexService) error {
	session, err := xconn.ConnectAnonymous(ctx, uri, common.LocalRealm)
	if err != nil {
		return fmt.Errorf("could not reach deskconnd: %w", err)
	}
	defer func() { _ = session.Leave() }()

	query := func(_ context.Context, inv *xconn.Invocation) *xconn.InvocationResult {
		return handleQuery(index, inv)
	}
	if resp := session.Register(common.ProcedureMediaIndexQuery, query).Do(); resp.Err != nil {
		return resp.Err
	}
	payload, err := json.Marshal(apps)
	if err != nil {
		return err
	}
	if resp := session.Call(common.ProcedureAppsRegister).Args(payload).Do(); resp.Err != nil {
		return fmt.Errorf("could not register apps: %w", resp.Err)
	}
	log.Println("media-app: connected to deskconnd")

	select {
	case <-ctx.Done():
		return nil
	case <-session.Done():
		return errors.New("disconnected from deskconnd")
	}
}

// handleQuery answers deskconnd's forwarded index query: its argument and result are JSON.
func handleQuery(index *IndexService, inv *xconn.Invocation) *xconn.InvocationResult {
	payload, err := inv.ArgBytes(0)
	if err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}
	var args struct {
		Categories []string          `json:"categories"`
		Cursor     map[string]string `json:"cursor"`
		Limit      int               `json:"limit"`
	}
	if err := json.Unmarshal(payload, &args); err != nil {
		return xconn.NewInvocationError(common.ErrInvalidArgument, err.Error())
	}

	result, err := index.Query(args.Categories, args.Cursor, args.Limit)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	data, err := json.Marshal(result)
	if err != nil {
		return xconn.NewInvocationError(common.ErrOperationFailed, err.Error())
	}
	return xconn.NewInvocationResult(data)
}
