package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	aiozimageoptimizer "github.com/lamgiahungaioz/aioz-image-optimizer"
	"github.com/shopspring/decimal"
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

	valid, isPremium, err := i.validateGatewayAccess(ctx, r, resolvedPath.RootCid().String())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return false
	}
	if !valid {
		http.Error(w, fmt.Sprintf("Gateway access denied for %s", resolvedPath.RootCid()), http.StatusForbidden)
		return false
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

		optimizerOpts := aiozimageoptimizer.DefaultOptions()
		width := r.URL.Query().Get("img-width")
		height := r.URL.Query().Get("img-height")
		animated := r.URL.Query().Get("img-anim")
		quality := r.URL.Query().Get("img-quality")
		dpr := r.URL.Query().Get("img-dpr")
		sharpen := r.URL.Query().Get("img-sharpen")
		fit := r.URL.Query().Get("img-fit")
		gravity := r.URL.Query().Get("img-gravity")
		onError := r.URL.Query().Get("img-onerror")
		metadata := r.URL.Query().Get("img-metadata")
		format := r.URL.Query().Get("img-format")

		shouldRedirectToSourceImg := onError == "redirect"
		shouldOptimize := width != "" ||
			height != "" ||
			animated != "" ||
			quality != "" ||
			dpr != "" ||
			sharpen != "" ||
			fit != "" ||
			format != ""

		// Optimize if options are provided
		if strings.HasPrefix(ctype, "image/") && shouldOptimize {
			var (
				err        error
				errMessage string
				code       int
			)
			defer func() {
				if len(errMessage) == 0 {
					return
				}
				if shouldRedirectToSourceImg {
					urlWithoutQuery := fmt.Sprintf("https://%s%s", r.Host, r.URL.Path)
					http.Redirect(w, r, urlWithoutQuery, http.StatusPermanentRedirect)
					return
				}
				http.Error(w, errMessage, code)
			}()
			if animated != "" && animated != "true" && animated != "false" {
				errMessage = "invalid value for animated"
				code = http.StatusBadRequest
				return false
			}
			if width != "" {
				parsedWidth, err := strconv.ParseUint(width, 10, 16) // 16-bit = 65535, should be enough
				if err != nil {
					errMessage = fmt.Sprintf("invalid value for width: %s", width)
					code = http.StatusBadRequest
					return false
				}
				optimizerOpts.Width = uint(parsedWidth)
			}
			if height != "" {
				parsedHeight, err := strconv.ParseUint(height, 10, 16) // 16-bit = 65535, should be enough
				if err != nil {
					errMessage = fmt.Sprintf("invalid value for height: %s", height)
					code = http.StatusBadRequest
					return false
				}
				optimizerOpts.Height = uint(parsedHeight)
			}
			if quality != "" {
				parsedQuality, err := strconv.ParseUint(quality, 10, 7) // 7-bit = 127, range should be 0-100
				if err != nil {
					errMessage = fmt.Sprintf("invalid value for quality: %s", quality)
					code = http.StatusBadRequest
					return false
				}
				parsedQualityUint := uint(parsedQuality)
				if parsedQualityUint > 100 {
					parsedQualityUint = 100
				}
				optimizerOpts.Quality = int(parsedQualityUint)
			}
			if dpr != "" {
				// Notes: DPR should be in range 1-3
				parsedDpr, err := strconv.ParseUint(dpr, 10, 32)
				if err != nil || parsedDpr == 0 {
					parsedDpr = 1
				}
				if parsedDpr > 3 {
					parsedDpr = 3
				}
				if parsedDpr != 1 {
					optimizerOpts.DevicePixelRatio = uint(parsedDpr)
				}
			}
			if sharpen != "" {
				parsedSharpen, err := strconv.ParseFloat(sharpen, 32)
				if err != nil {
					errMessage = fmt.Sprintf("invalid value for sharpen: %s", sharpen)
					code = http.StatusBadRequest
					return false
				}
				if parsedSharpen < 0 {
					parsedSharpen = 0
				}
				if parsedSharpen > 10 {
					parsedSharpen = 10
				}
				optimizerOpts.Sharpen = parsedSharpen
			}
			if format != "" {
				switch format {
				case "webp":
					acceptHeader := r.Header.Get("Accept")
					optimizerOpts.ShouldTransformToWebp = strings.Contains(acceptHeader, "image/webp")
				default:
					optimizerOpts.ShouldTransformToWebp = false
				}
			}
			if gravity != "" {
				switch gravity {
				case "auto":
					optimizerOpts.AutoDetermineGravity = true
				case "left":
					optimizerOpts.GravitySide = aiozimageoptimizer.GravityLeft
				case "right":
					optimizerOpts.GravitySide = aiozimageoptimizer.GravityRight
				case "top":
					optimizerOpts.GravitySide = aiozimageoptimizer.GravityTop
				case "bottom":
					optimizerOpts.GravitySide = aiozimageoptimizer.GravityBottom
				default:
					if !strings.Contains(gravity, "x") {
						errMessage = fmt.Sprintf("invalid value for gravity: %s", gravity)
						code = http.StatusBadRequest
						return false
					}
					parts := strings.Split(gravity, "x")
					if len(parts) != 2 {
						errMessage = fmt.Sprintf("invalid value for gravity: %s", gravity)
						code = http.StatusBadRequest
						return false
					}
					widthGravityStr := parts[0]
					heightGravityStr := parts[1]

					parsedWidthGravity, err := strconv.ParseFloat(widthGravityStr, 32)
					if err != nil {
						errMessage = fmt.Sprintf("invalid value for width gravity: %s", widthGravityStr)
						code = http.StatusBadRequest
						return false
					}
					optimizerOpts.WidthGravity = float32(parsedWidthGravity)

					parsedHeightGravity, err := strconv.ParseFloat(heightGravityStr, 32)
					if err != nil {
						errMessage = fmt.Sprintf("invalid value for height gravity: %s", heightGravityStr)
						code = http.StatusBadRequest
						return false
					}
					optimizerOpts.HeightGravity = float32(parsedHeightGravity)
				}
			}

			if metadata != "" {
				switch metadata {
				case "keep":
					optimizerOpts.MetadataKeepMode = aiozimageoptimizer.MetadataKeep
				case "copyright":
					optimizerOpts.MetadataKeepMode = aiozimageoptimizer.MetadataCopyright
				case "none":
					optimizerOpts.MetadataKeepMode = aiozimageoptimizer.MetadataNone
				default:
					errMessage = fmt.Sprintf("invalid value for metadata: %s", metadata)
					code = http.StatusBadRequest
					return false
				}
			}

			switch fit {
			case "scale-down":
				optimizerOpts.Fit = aiozimageoptimizer.FitModeScaleDown
			case "contain":
				optimizerOpts.Fit = aiozimageoptimizer.FitModeContain
			case "cover":
				optimizerOpts.Fit = aiozimageoptimizer.FitModeCover
			case "crop":
				optimizerOpts.Fit = aiozimageoptimizer.FitModeCrop
			case "pad":
				optimizerOpts.Fit = aiozimageoptimizer.FitModePad
			default:
				optimizerOpts.Fit = aiozimageoptimizer.FitModeScaleDown
			}
			optimizerOpts.PreserveAnimation = animated == "true"
			optimizer := aiozimageoptimizer.OptimizerFromMimeType(ctype)
			content, err = optimizer.Optimize(content, optimizerOpts)
			if err != nil {
				errMessage = fmt.Sprintf("error optimizing content: %s", err.Error())
				code = http.StatusInternalServerError
				return false
			}

			if optimizerOpts.ShouldTransformToWebp {
				ctype = "image/webp"
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

	limitReader := RateLimitReader(isPremium, content)

	// ServeContent will take care of
	// If-None-Match+Etag, Content-Length and range requests
	_, dataSent, _ := serveContent(w, r, modtime, fileSize, limitReader)

	// Was response successful?
	if dataSent {
		// Update metrics
		i.unixfsFileGetMetric.WithLabelValues(contentPath.Namespace()).Observe(time.Since(begin).Seconds())
		i.addBandwidthUsage(r, resolvedPath.RootCid().String(), uint64(fileSize))
	}

	return dataSent
}

func (i *handler) validateGatewayAccess(ctx context.Context, r *http.Request, rootCid string) (bool, bool, error) {
	type ValidateGatewayAccessRequest struct {
		GatewayName   string `json:"gatewayName"`
		CID           string `json:"cid"`
		AccessKey     string `json:"accessKey"`
		RequestIp     string `json:"ip"`
		RequestOrigin string `json:"origin"`
	}
	// api.example.com, domain: example.com => subdomain 'api'
	subdomain := strings.TrimSuffix(r.Host, fmt.Sprintf(".%s", i.domain))
	// access-key can be from header or url query
	accessKey := r.Header.Get("x-aiozpin-gateway-token")
	if len(accessKey) == 0 {
		accessKey = r.URL.Query().Get("aiozpinGatewayToken")
	}
	requestIp := strings.Split(r.RemoteAddr, ":")[0] // RemoteAddr is IP:port, this line removes the port
	requestOrigin := r.Header.Get("Origin")
	if len(requestOrigin) == 0 {
		requestOrigin = r.Header.Get("Referer")
	}

	validateRequest := &ValidateGatewayAccessRequest{
		GatewayName:   subdomain,
		CID:           rootCid,
		AccessKey:     accessKey,
		RequestIp:     requestIp,
		RequestOrigin: requestOrigin,
	}
	reqAsJson, err := json.Marshal(validateRequest)
	if err != nil {
		return false, false, err
	}
	validateEndpoint := fmt.Sprintf("%s/api/gatewayAccessControlRules/validate", i.pinningApiEndpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, validateEndpoint, bytes.NewBuffer(reqAsJson))
	if err != nil {
		return false, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("blockservice-API-Key", i.blockServiceApiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, false, err
	}
	defer resp.Body.Close()
	type AccessResponse struct {
		Data struct {
			Valid     bool `json:"valid"`
			IsPremium bool `json:"isPremium"`
		} `json:"data"`
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	var response AccessResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return false, false, err
	}
	if response.Status == "success" {
		return response.Data.Valid, response.Data.IsPremium, nil
	}
	return false, false, errors.New(response.Message)
}

func (i *handler) addBandwidthUsage(r *http.Request, rootCid string, fileSize uint64) {
	type AddBandwidthRequest struct {
		CID     string          `json:"cid" binding:"required"`
		Gateway string          `json:"gateway" binding:"required"`
		Amount  decimal.Decimal `gorm:"type:numeric" json:"amount" binding:"required"`
	}

	// api.example.com, domain: example.com => subdomain 'api'
	subdomain := strings.TrimSuffix(r.Host, fmt.Sprintf(".%s", i.domain))
	addBandwidthRequest := AddBandwidthRequest{
		CID:     rootCid,
		Amount:  decimal.NewFromUint64(fileSize),
		Gateway: subdomain,
	}

	if err := i.rabbitMQ.Publish(addBandwidthRequest); err != nil {
		log.Errorf("failed to publish AddBandwidthRequest: %s", err.Error())
	}
}
