package gateway

import (
	"context"
	"encoding/gob"
	"fmt"
	"net/http"
	"net/url"
	"os"
	gopath "path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/ipfs/boxo/gateway/assets"
	"github.com/ipfs/boxo/path"
	cid "github.com/ipfs/go-cid"
	"go.uber.org/zap"
)

var mutexLock sync.Mutex

// serveDirectory returns the best representation of UnixFS directory
//
// It will return index.html if present, or generate directory listing otherwise.
func (i *handler) serveDirectory(ctx context.Context, w http.ResponseWriter, r *http.Request, resolvedPath path.ImmutablePath, contentPath path.Path, isHeadRequest bool, dirMetadata *directoryMetadata, ranges []ByteRange, begin time.Time, logger *zap.SugaredLogger) bool {
	// ctx, span := spanTrace(ctx, "Handler.ServeDirectory", trace.WithAttributes(attribute.String("path", resolvedPath.String())))
	// defer span.End()

	// WithHostname might have constructed an IPNS/IPFS path using the Host header.
	// In this case, we need the original path for constructing redirects and links
	// that match the requested URL.
	// For example, http://example.net would become /ipns/example.net, and
	// the redirects and links would end up as http://example.net/ipns/example.net
	requestURI, err := url.ParseRequestURI(r.RequestURI)
	if err != nil {
		i.webError(w, r, fmt.Errorf("failed to parse request path: %w", err), http.StatusInternalServerError)
		return false
	}
	originalURLPath := requestURI.Path

	// Ensure directory paths end with '/'
	if originalURLPath[len(originalURLPath)-1] != '/' {
		// don't redirect to trailing slash if it's go get
		// https://github.com/ipfs/kubo/pull/3963
		goget := r.URL.Query().Get("go-get") == "1"
		if !goget {
			suffix := "/"
			// preserve query parameters
			if r.URL.RawQuery != "" {
				suffix = suffix + "?" + r.URL.RawQuery
			}
			// /ipfs/cid/foo?bar must be redirected to /ipfs/cid/foo/?bar
			redirectURL := originalURLPath + suffix
			logger.Debugw("directory location moved permanently", "status", http.StatusMovedPermanently)
			http.Redirect(w, r, redirectURL, http.StatusMovedPermanently)
			return true
		}
	}

	// A HTML directory index will be presented, be sure to set the correct
	// type instead of relying on autodetection (which may fail).
	w.Header().Set("Content-Type", "text/html")

	// Generated dir index requires custom Etag (output may change between go-libipfs versions)
	dirEtag := getDirListingEtag(resolvedPath.RootCid())
	w.Header().Set("Etag", dirEtag)

	if r.Method == http.MethodHead {
		logger.Debug("return as request's HTTP method is HEAD")
		return true
	}

	var dirListing []assets.DirectoryItem
	errorCount := 0
	fileName := `DirCacheMetadata-` + `CID-` + resolvedPath.RootCid().String()
	fileDir := filepath.Join("public", "DirCacheMetadata")

	if err := os.MkdirAll(fileDir, os.ModePerm); err != nil {
		fmt.Println("Error creating directory: ", err)
	}
	dirCacheMetadata := filepath.Join(fileDir, fileName)

	if _, err := os.Stat(dirCacheMetadata); err == nil {
		// File exists, load dirListing from the file
		file, err := os.Open(dirCacheMetadata)
		if err != nil {
			fmt.Println("Error opening file: ", err)
			return false
		}
		defer file.Close()

		dec := gob.NewDecoder(file)
		if err = dec.Decode(&dirListing); err != nil {
			fmt.Println("Error decoding dirListing: ", err)
			// If there was an error decoding the file, delete it and create a new dirListing
			if err := os.Remove(dirCacheMetadata); err != nil {
				fmt.Println("Error removing file: ", err)
				return false
			}
			return false
		}

		for i, l := range dirListing {
			dirListing[i].Path = gopath.Join(originalURLPath, l.Name)
		}

	} else if os.IsNotExist(err) {
		// File does not exist, create dirListing
		mutexLock.Lock()
		defer mutexLock.Unlock()

		for l := range dirMetadata.entries {
			if l.Err != nil {
				errorCount++
				if errorCount > len(dirListing)/1000 {
					go func(dirMetadata *directoryMetadata) {
						for i := 0; i < 5; i++ {
							if err := CacheMetadata(dirMetadata, resolvedPath, originalURLPath); err != nil {
								fmt.Println("Error caching metadata: ", err)
								continue
							}
							return
						}
					}(dirMetadata)
					i.webError(w, r, l.Err, http.StatusInternalServerError)
					return false
				}
				continue
			}

			name := l.Link.Name
			sz := l.Link.Size
			linkCid := l.Link.Cid

			hash := linkCid.String()
			di := assets.DirectoryItem{
				Size:      humanize.Bytes(sz),
				Name:      name,
				Path:      gopath.Join(originalURLPath, name),
				Hash:      hash,
				ShortHash: assets.ShortHash(hash),
			}
			dirListing = append(dirListing, di)
		}
		sort.Slice(dirListing, func(i, j int) bool {
			return dirListing[i].Name < dirListing[j].Name
		})

		// If there were no errors and dirListing is large, save it to a file
		if errorCount == 0 && len(dirListing) > 200 {
			fmt.Println("Saving dirListing to file")
			file, err := os.Create(dirCacheMetadata)
			if err != nil {
				fmt.Println("Error creating file: ", err)
				return false
			}
			defer file.Close()

			enc := gob.NewEncoder(file)
			if err = enc.Encode(dirListing); err != nil {
				fmt.Println("Error encoding dirListing: ", err)
			}
		}

		if errorCount > 0 && len(dirListing) > 200 {
			go func(dirMetadata *directoryMetadata) {
				for i := 0; i < 5; i++ {
					if err := CacheMetadata(dirMetadata, resolvedPath, originalURLPath); err != nil {
						fmt.Println("Error caching metadata: ", err)
						continue
					}
					return
				}
			}(dirMetadata)
		}
	}

	// construct the correct back link
	// https://github.com/ipfs/kubo/issues/1365
	backLink := originalURLPath

	// don't go further up than /ipfs/$hash/
	pathSplit := strings.Split(contentPath.String(), "/")
	switch {
	// skip backlink when listing a content root
	case len(pathSplit) == 3: // url: /ipfs/$hash
		backLink = ""

	// skip backlink when listing a content root
	case len(pathSplit) == 4 && pathSplit[3] == "": // url: /ipfs/$hash/
		backLink = ""

	// add the correct link depending on whether the path ends with a slash
	default:
		if strings.HasSuffix(backLink, "/") {
			backLink += ".."
		} else {
			backLink += "/.."
		}
	}

	size := humanize.Bytes(dirMetadata.dagSize)
	hash := resolvedPath.RootCid().String()
	globalData := i.getTemplateGlobalData(r, contentPath)

	// See comment above where originalUrlPath is declared.
	tplData := assets.DirectoryTemplateData{
		GlobalData:  globalData,
		Listing:     dirListing,
		Size:        size,
		Path:        contentPath.String(),
		GatewayURL:  globalData.GatewayURL,
		Breadcrumbs: assets.Breadcrumbs(contentPath.String(), globalData.DNSLink),
		BackLink:    backLink,
		Hash:        hash,
	}

	logger.Debugw("request processed", "tplDataDNSLink", globalData.DNSLink, "tplDataSize", size, "tplDataBackLink", backLink, "tplDataHash", hash)

	if err := assets.DirectoryTemplate.Execute(w, tplData); err != nil {
		i.webError(w, r, err, http.StatusInternalServerError)
		return false
	}

	// Update metrics
	i.unixfsGenDirListingGetMetric.WithLabelValues(contentPath.Namespace()).Observe(time.Since(begin).Seconds())
	return true
}

func getDirListingEtag(dirCid cid.Cid) string {
	return `"DirIndex-` + assets.AssetHash + `_CID-` + dirCid.String() + `"`
}

func CacheMetadata(dirMetadata *directoryMetadata, resolvedPath path.ImmutablePath, originalURLPath string) error {
	var dirListing []assets.DirectoryItem
	fileName := `DirCacheMetadata-` + `CID-` + resolvedPath.RootCid().String()
	fileDir := filepath.Join("public", "DirCacheMetadata")

	if err := os.MkdirAll(fileDir, os.ModePerm); err != nil {
		fmt.Println("Error creating directory: ", err)
	}
	dirCacheMetadata := filepath.Join(fileDir, fileName)
	if _, err := os.Stat(dirCacheMetadata); err == nil {
		// File exists, load dirListing from the file
		file, err := os.Open(dirCacheMetadata)
		if err != nil {
			fmt.Println("Error opening file: ", err)
			return err
		}
		defer file.Close()

		dec := gob.NewDecoder(file)
		if err = dec.Decode(&dirListing); err != nil {
			fmt.Println("Error decoding dirListing: ", err)
			// If there was an error decoding the file, delete it and create a new dirListing
			if err := os.Remove(dirCacheMetadata); err != nil {
				fmt.Println("Error removing file: ", err)
				return err
			}
			return err
		}

		return nil

	} else if os.IsNotExist(err) {
		for l := range dirMetadata.entries {
			if l.Err != nil {
				return l.Err
			}

			name := l.Link.Name
			sz := l.Link.Size
			linkCid := l.Link.Cid

			hash := linkCid.String()
			di := assets.DirectoryItem{
				Size:      humanize.Bytes(sz),
				Name:      name,
				Path:      gopath.Join(originalURLPath, name),
				Hash:      hash,
				ShortHash: assets.ShortHash(hash),
			}
			dirListing = append(dirListing, di)
		}
		sort.Slice(dirListing, func(i, j int) bool {
			return dirListing[i].Name < dirListing[j].Name
		})

		// If there were no errors and dirListing is large, save it to a file
		if len(dirListing) > 200 {
			fmt.Println("Saving dirListing to file")
			file, err := os.Create(dirCacheMetadata)
			if err != nil {
				fmt.Println("Error creating file: ", err)
				return err
			}
			defer file.Close()

			enc := gob.NewEncoder(file)
			if err = enc.Encode(dirListing); err != nil {
				fmt.Println("Error encoding dirListing: ", err)
			}
		}
	}
	return nil
}
