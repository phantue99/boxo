package gateway

import (
	"bytes"
	"context"
	"fmt"
	"github.com/ipfs/boxo/images"
	"io"
	"mime"
	"net/http"
	gopath "path"
	"strconv"
	"strings"
	"time"

	"github.com/gabriel-vasile/mimetype"
	"github.com/ipfs/boxo/path"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// serveFile returns data behind a file along with HTTP headers based on
// the file itself, its CID and the contentPath used for accessing it.
func (i *handler) serveFile(ctx context.Context, w http.ResponseWriter, r *http.Request, resolvedPath path.ImmutablePath, contentPath path.Path, fileSize int64, fileBytes io.ReadCloser, isSymlink bool, returnRangeStartsAtZero bool, fileContentType string, begin time.Time) bool {
	_, span := spanTrace(ctx, "Handler.ServeFile", trace.WithAttributes(attribute.String("path", resolvedPath.String())))
	defer span.End()

	// Set Cache-Control and read optional Last-Modified time
	modtime := addCacheControlHeaders(w, r, contentPath, resolvedPath.RootCid(), "")

	// Set Content-Disposition
	name := addContentDispositionHeader(w, r, contentPath)

	if fileSize == 0 {
		// We override null files to 200 to avoid issues with fragment caching reverse proxies.
		// Also whatever you are asking for, it's cheaper to just give you the complete file (nothing).
		// TODO: remove this if clause once https://github.com/golang/go/issues/54794 is fixed in two latest releases of go
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		return true
	}

	var content io.Reader = fileBytes
	// Calculate deterministic value for Content-Type HTTP header
	// (we prefer to do it here, rather than using implicit sniffing in http.ServeContent)
	var ctype string
	if isSymlink {
		// We should be smarter about resolving symlinks but this is the
		// "most correct" we can be without doing that.
		ctype = "inode/symlink"
	} else {
		ctype = mime.TypeByExtension(gopath.Ext(name))
		if ctype == "" {
			ctype = fileContentType
		}
		if ctype == "" && returnRangeStartsAtZero {
			// uses https://github.com/gabriel-vasile/mimetype library to determine the content type.
			// Fixes https://github.com/ipfs/kubo/issues/7252

			// We read from a TeeReader into a buffer and then put the buffer in front of the original reader to
			// simulate the behavior of being able to read from the start and then seek back to the beginning while
			// only having a Reader and not a ReadSeeker
			var buf bytes.Buffer
			tr := io.TeeReader(fileBytes, &buf)

			mimeType, err := mimetype.DetectReader(tr)
			if err != nil {
				http.Error(w, fmt.Sprintf("cannot detect content-type: %s", err.Error()), http.StatusInternalServerError)
				return false
			}

			ctype = mimeType.String()
			content = io.MultiReader(&buf, fileBytes)
		}

		width := r.URL.Query().Get("width")
		height := r.URL.Query().Get("height")
		animated := r.URL.Query().Get("animated")
		shouldResize := width != "" || height != ""
		// Resize and scale if options are provided
		if strings.HasPrefix(ctype, "image/") && shouldResize {
			var (
				widthVal, heightVal *uint
				animatedVal         bool
				err                 error
			)

			if animated != "" && animated != "true" && animated != "false" {
				http.Error(w, fmt.Sprintf("invalid value for animated"), http.StatusBadRequest)
				return false
			}
			if width != "" {
				parsedWidth, err := strconv.ParseUint(width, 10, 16)
				if err != nil {
					http.Error(w, fmt.Sprintf("invalid value for width: %s", width), http.StatusBadRequest)
					return false
				}
				parsedWidthUint := uint(parsedWidth)
				widthVal = &parsedWidthUint
			}
			if height != "" {
				parsedHeight, err := strconv.ParseUint(height, 10, 16)
				if err != nil {
					http.Error(w, fmt.Sprintf("invalid value for height: %s", height), http.StatusBadRequest)
					return false
				}
				parsedHeightUint := uint(parsedHeight)
				heightVal = &parsedHeightUint
			}

			animatedVal = animated == "true"
			resizer := images.ResizerFromMimeType(ctype)
			content, err = resizer.Resize(content, widthVal, heightVal, animatedVal)
			if err != nil {
				http.Error(w, fmt.Sprintf("cannot resize image: %s", err.Error()), http.StatusInternalServerError)
				return false
			}
		}
		// Strip the encoding from the HTML Content-Type header and let the
		// browser figure it out.
		//
		// Fixes https://github.com/ipfs/kubo/issues/2203
		if strings.HasPrefix(ctype, "text/html;") {
			ctype = "text/html"
		}
	}
	// Setting explicit Content-Type to avoid mime-type sniffing on the client
	// (unifies behavior across gateways and web browsers)
	w.Header().Set("Content-Type", ctype)

	limitReader := RateLimitReader(i.isDedicatedGateway, content)

	// ServeContent will take care of
	// If-None-Match+Etag, Content-Length and range requests
	_, dataSent, _ := serveContent(w, r, modtime, fileSize, limitReader)

	// Was response successful?
	if dataSent {
		// Update metrics
		i.unixfsFileGetMetric.WithLabelValues(contentPath.Namespace()).Observe(time.Since(begin).Seconds())
	}

	return dataSent
}
