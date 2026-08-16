// Command video-example serves a directory of video files with HTTP range
// support, and a Breeze-templated page that plays them.
//
// Run it from this directory so the template engine finds ./views:
//
//	cd cmd/video-example && go run .
//
// It creates ./media on first run. Drop an .mp4 in there, open
// http://localhost:3000, and press play — seeking in the scrubber issues
// range requests, which is the behaviour this package exists to provide.
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"time"

	breeze "github.com/nelthaarion/breeze"
	"github.com/nelthaarion/breeze/dashboard"
	"github.com/nelthaarion/breeze/events"
	"github.com/nelthaarion/breeze/video"
)

// chunkSize is deliberately small so the demo shows several writes per
// request in the log. 256 KiB (the package default) is the right
// production value.
const chunkSize = 128 << 10

// clip is one row on the page.
type clip struct {
	Name string
	URL  string
}

func main() {
	root := "./media"
	if err := os.MkdirAll(root, 0o755); err != nil {
		log.Fatalf("creating %s: %v", root, err)
	}
	abs, _ := filepath.Abs(root)

	// A signing secret turns every URL into a capability that expires.
	// Hardcoding one in an example would be fine; leaving it in the
	// environment means the demo works out of the box and only enforces
	// signatures when you ask it to.
	secret := []byte(os.Getenv("VIDEO_SECRET"))
	signed := len(secret) > 0

	router := breeze.NewRouter()

	engine := breeze.NewTemplateEngine(breeze.TemplateConfig{
		ViewsDir:   "./views",
		LayoutFile: "./views/layout.html",
		DevMode:    true,
	})

	if err := video.Mount(router, video.Config{
		Root:         root,
		Prefix:       "/videos",
		ChunkSize:    chunkSize,
		Secret:       secret,
		MaxChunkSize: chunkSize * 2,

		// Report the internal error, which is never sent to the client.
		OnError: func(ctx *breeze.Context, name string, err error) {
			log.Printf("video: %s: %v", name, err)
		},
	}); err != nil {
		log.Fatalf("mounting video: %v", err)
	}

	// Listening for the package's own event shows how a viewer's progress
	// can be tracked without touching the streaming path.
	events.OnType(func(_ *events.Context, e video.StreamServed) error {
		switch {
		case e.Disconnected:
			log.Printf("stream %s: viewer left after %d bytes", e.File, e.Bytes)
		default:
			log.Printf("stream %s: %d → %d bytes in %v",
				e.File, e.Status, e.Bytes, e.Duration.Round(time.Millisecond))
		}
		return nil
	})

	router.Handle(breeze.GET, "/", func(ctx *breeze.Context) {
		names, err := listVideos(root)
		if err != nil {
			ctx.Status(500)
			ctx.WriteString("cannot read media directory")
			return
		}

		clips := make([]clip, 0, len(names))
		for _, n := range names {
			u := "/videos/" + n
			if signed {
				// A short window is the point: a link that leaks is only
				// useful for as long as it lives.
				u += "?" + video.Sign(secret, n, 10*time.Minute)
			}
			clips = append(clips, clip{Name: n, URL: u})
		}

		engine.RenderView(ctx, "home", map[string]any{
			"Videos":    clips,
			"Root":      abs,
			"ChunkSize": fmt.Sprintf("%d KiB", chunkSize>>10),
			"Signed":    signed,
		})
	})

	fmt.Println("video-example listening on http://localhost:3000")
	fmt.Println("  serving:", abs)
	if signed {
		fmt.Println("  signed URLs: ON (VIDEO_SECRET is set)")
	} else {
		fmt.Println("  signed URLs: off — set VIDEO_SECRET to require them")
	}

	app := breeze.New(router, breeze.NewEventLoopWorkerPool(runtime.NumCPU()))

	// The dashboard's Video tab groups traffic by file, which is the only
	// useful view of a stream: one viewer produces hundreds of range
	// requests, so the Live Requests page shows the same film over and
	// over while answering nothing about bandwidth.
	coll := dashboard.Install(app, router, dashboard.Config{
		Username: "admin",
		Password: "admin",
	})

	// The same bus video.Mount published on. Mount defaults to
	// events.Default when Config.Bus is unset, so that is what gets
	// attached here.
	defer coll.AttachVideo(events.Default)()

	fmt.Println("  dashboard: http://localhost:3000/dashboard/video (admin/admin)")

	app.Run(3000, true)
}

// listVideos returns the playable files at the top level of root.
func listVideos(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch filepath.Ext(e.Name()) {
		case ".mp4", ".webm", ".ogv", ".m4v", ".mov", ".mkv":
			out = append(out, e.Name())
		}
	}
	return out, nil
}
