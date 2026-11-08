// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Command grpcclient is another app running a notebook in nbsandbox over
// gRPC: upload a notebook and its data, start a run with parameters, follow
// its result as it is made (item by item, for a notebook that streams its
// output), and print what it returned. It uses the generated client in
// github.com/azbyluthfan/nbsandbox/gen.
//
//	export NBSANDBOX_URL=http://127.0.0.1:8080
//	export NBSANDBOX_TOKEN=...            # a bearer token issued for nbsandbox
//	# or: export NBSANDBOX_USER=alice NBSANDBOX_PASSWORD=alice-dev-password
//	go run ./examples/go/grpcclient -region bandung -months 4
//	go run ./examples/go/grpcclient -stream          # items as they are made
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/azbyluthfan/nbsandbox/gen/nbsandbox/v1"
	"github.com/azbyluthfan/nbsandbox/gen/nbsandbox/v1/nbsandboxv1connect"
)

// credentials adds the Authorization metadata to every call.
type credentials string

func (c credentials) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
		r.Header().Set("Authorization", string(c))
		return next(ctx, r)
	}
}

func (c credentials) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, s connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, s)
		conn.RequestHeader().Set("Authorization", string(c))
		return conn
	}
}

func (c credentials) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func authorization() string {
	if t := os.Getenv("NBSANDBOX_TOKEN"); t != "" {
		return "Bearer " + t
	}
	up := os.Getenv("NBSANDBOX_USER") + ":" + os.Getenv("NBSANDBOX_PASSWORD")
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(up))
}

func main() {
	region := flag.String("region", "jakarta", "jakarta, bandung or surabaya")
	months := flag.Int("months", 3, "how many recent months to summarise")
	stream := flag.Bool("stream", false, "run sales_by_month.ipynb, which streams one item per month, instead of sales_report.ipynb")
	dir := flag.String("notebooks", "examples/notebooks", "folder with the notebooks and data/")
	flag.Parse()
	notebook := "sales_report.ipynb"
	if *stream {
		notebook = "sales_by_month.ipynb"
	}
	base := os.Getenv("NBSANDBOX_URL")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	// gRPC runs over HTTP/2: over TLS for https://, cleartext (h2c) for
	// http:// (a local stack, or behind a proxy that terminates TLS).
	tr := &http.Transport{Protocols: new(http.Protocols)}
	tr.Protocols.SetHTTP2(true)
	tr.Protocols.SetUnencryptedHTTP2(true)
	hc := &http.Client{Transport: tr}
	opts := []connect.ClientOption{connect.WithGRPC(), connect.WithInterceptors(credentials(authorization()))}
	spaces := nbsandboxv1connect.NewSpaceServiceClient(hc, base, opts...)
	runs := nbsandboxv1connect.NewRunServiceClient(hc, base, opts...)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	sp, err := spaces.CreateSpace(ctx, connect.NewRequest(&pb.CreateSpaceRequest{Name: "example " + time.Now().Format("2006-01-02 15:04")}))
	if err != nil {
		log.Fatalf("create space: %v", err)
	}
	sid := sp.Msg.Id
	fmt.Println("space", sid)

	// One stream per folder: the target, then each file's name and data.
	for folder, files := range map[string][]string{"": {notebook}, "data": {"data/sales.csv"}} {
		up := spaces.UploadFiles(ctx)
		send := func(m *pb.UploadFilesRequest) {
			if err := up.Send(m); err != nil {
				log.Fatalf("upload: %v", err)
			}
		}
		send(&pb.UploadFilesRequest{Part: &pb.UploadFilesRequest_Target{Target: &pb.UploadTarget{SpaceId: sid, Folder: folder}}})
		for _, f := range files {
			b, err := os.ReadFile(filepath.Join(*dir, f)) //nolint:gosec // the example's own files
			if err != nil {
				log.Fatal(err)
			}
			send(&pb.UploadFilesRequest{Part: &pb.UploadFilesRequest_FileName{FileName: filepath.Base(f)}})
			for len(b) > 0 { // in chunks, as a large file would go
				n := min(len(b), 64<<10)
				send(&pb.UploadFilesRequest{Part: &pb.UploadFilesRequest_Data{Data: b[:n]}})
				b = b[n:]
			}
		}
		if _, err := up.CloseAndReceive(); err != nil {
			log.Fatalf("upload: %v", err)
		}
	}

	params, _ := structpb.NewValue(map[string]any{"region": *region, "months": *months})
	key := make([]byte, 8)
	_, _ = rand.Read(key)
	run, err := runs.CreateRun(ctx, connect.NewRequest(&pb.CreateRunRequest{SpaceId: sid, Notebook: notebook,
		Parameters: params, IdempotencyKey: hex.EncodeToString(key)}))
	if err != nil {
		log.Fatalf("start run: %v", err) // INVALID_ARGUMENT: the contract refused the parameters
	}
	fmt.Println("run", run.Msg.Id, run.Msg.Status)

	// The result as it is made: each item of a streamed output as soon as it
	// is ready, then the result. If the stream drops, follow again with After
	// set to the last index seen, so no item is handled twice.
	follow, err := runs.FollowRunResult(ctx, connect.NewRequest(&pb.FollowRunResultRequest{SpaceId: sid, RunId: run.Msg.Id}))
	if err != nil {
		log.Fatalf("follow: %v", err)
	}
	var r *pb.Result
	for follow.Receive() {
		switch ev := follow.Msg().Event.(type) {
		case *pb.ResultEvent_Item:
			b, _ := json.Marshal(ev.Item.Item.AsInterface())
			fmt.Printf("  item %d: %s\n", ev.Item.Index, b) // a chart would add a point here
		case *pb.ResultEvent_Result:
			r = ev.Result
		}
	}
	if err := follow.Err(); err != nil || r == nil {
		log.Fatalf("follow: %v", err)
	}
	if r.Status != "succeeded" {
		if r.Partial {
			b, _ := json.Marshal(r.Output.AsInterface())
			fmt.Printf("items before it failed: %s\n", b) // kept; run again for the rest
		}
		log.Fatalf("run %s: %s (%s fault, retryable %v)", r.Status, r.Error.GetMessage(), r.Error.GetFault(), r.Error.GetRetryable()) //nolint:gosec // an example printing the server's answer
	}
	out, _ := json.MarshalIndent(r.Output.AsInterface(), "", "  ")
	fmt.Println(string(out))
}
