package slackbridge

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
	log "github.com/sirupsen/logrus"
)

// maxDownloads caps the image downloads from Slack in progress at once;
// each holds up to agentbus.MaxImageBytes in memory.
const maxDownloads = 2

// pendingImages is what DeliverViaImages needs of the files shared with a
// message, in the same order. The Store decides which are relayed (type,
// size, count); a file without a download link is only listed.
func pendingImages(files []slackFile) []agentbus.PendingImage {
	out := make([]agentbus.PendingImage, 0, len(files))
	for _, f := range files {
		p := agentbus.PendingImage{Name: f.Name, Mime: strings.ToLower(strings.TrimSpace(f.Mimetype)), Size: f.Size}
		if p.Name == "" {
			p.Name = f.ID
		}
		if strings.TrimSpace(f.URLPrivateDownload) == "" && agentbus.IsRelayedImageType(p.Mime) {
			p.Error = "Slack gave no download link for it"
		}
		out = append(out, p)
	}
	return out
}

// fetchImages downloads, in the background, each image refs says was
// relayed (an ID), from the file at the same index in files, and hands the
// bytes or the failure to the Store. It never touches the network itself,
// so handleEvent may call it.
func (b *Bridge) fetchImages(refs []agentbus.ImageRef, files []slackFile) {
	type fetch struct{ id, url string }
	var todo []fetch
	for i, ref := range refs {
		if ref.ID != "" && i < len(files) {
			todo = append(todo, fetch{ref.ID, files[i].URLPrivateDownload})
		}
	}
	if len(todo) == 0 {
		return
	}
	ctx := b.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	b.downloadsWG.Add(1)
	go func() {
		defer b.downloadsWG.Done()
		for _, f := range todo {
			select {
			case b.downloadSlots <- struct{}{}:
			case <-ctx.Done():
				b.bus.FailImage(f.id, "the proxy stopped before the download")
				continue
			}
			data, err := b.api.downloadFile(ctx, b.cfg.BotToken, f.url, agentbus.MaxImageBytes)
			<-b.downloadSlots
			if err != nil {
				log.Infof("slack: image download for agentbus failed: %v", err)
				b.bus.FailImage(f.id, err.Error())
				continue
			}
			b.bus.FinishImage(f.id, data)
		}
	}()
}
