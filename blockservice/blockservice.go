// Package blockservice implements a BlockService interface that provides
// a single GetBlock/AddBlock interface that seamlessly retrieves data either
// locally or from a remote peer through the exchange.
package blockservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/exchange"
	"github.com/ipfs/boxo/rabbitmq"
	"github.com/ipfs/boxo/verifcid"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"
	logging "github.com/ipfs/go-log/v2"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"github.com/ipfs/boxo/blockservice/internal"
)

var logger = logging.Logger("blockservice")

// BlockGetter is the common interface shared between blockservice sessions and
// the blockservice.
type BlockGetter interface {
	// GetBlock gets the requested block.
	GetBlock(ctx context.Context, c cid.Cid) (blocks.Block, error)

	// GetBlocks does a batch request for the given cids, returning blocks as
	// they are found, in no particular order.
	//
	// It may not be able to find all requested blocks (or the context may
	// be canceled). In that case, it will close the channel early. It is up
	// to the consumer to detect this situation and keep track which blocks
	// it has received and which it hasn't.
	GetBlocks(ctx context.Context, ks []cid.Cid) <-chan blocks.Block
}

// BlockService is a hybrid block datastore. It stores data in a local
// datastore and may retrieve data from a remote Exchange.
// It uses an internal `datastore.Datastore` instance to store values.
type BlockService interface {
	io.Closer
	BlockGetter

	// Blockstore returns a reference to the underlying blockstore
	Blockstore() blockstore.Blockstore

	// Exchange returns a reference to the underlying exchange (usually bitswap)
	Exchange() exchange.Interface

	// AddBlock puts a given block to the underlying datastore
	AddBlock(ctx context.Context, o blocks.Block) error

	// AddBlocks adds a slice of blocks at the same time using batching
	// capabilities of the underlying datastore whenever possible.
	AddBlocks(ctx context.Context, bs []blocks.Block) error

	// DeleteBlock deletes the given block from the blockservice.
	DeleteBlock(ctx context.Context, o cid.Cid) error
}

// BoundedBlockService is a Blockservice bounded via strict multihash Allowlist.
type BoundedBlockService interface {
	BlockService

	Allowlist() verifcid.Allowlist
}

type blockService struct {
	allowlist  verifcid.Allowlist
	blockstore blockstore.Blockstore
	exchange   exchange.Interface
	// If checkFirst is true then first check that a block doesn't
	// already exist to avoid republishing the block on the exchange.
	checkFirst bool
}

type fileRecord struct {
	FileRecordID string
	Size         uint64
}

type fileInfo struct {
	FileRecordID string
	Size         uint64
	Offset       uint64
}

type ZipHeader struct {
	File []File
}
type File struct {
	Name               string `json:"Name"`
	CompressedSize     uint32 `json:"CompressedSize"`
	UncompressedSize   uint32 `json:"UncompressedSize"`
	CompressedSize64   uint64 `json:"CompressedSize64"`
	UncompressedSize64 uint64 `json:"UncompressedSize64"`
	Offset             uint64 `json:"Offset"`
}

var (
	uploader           string
	pinningService     string
	isDedicatedGateway bool
	maxSize            = 100 * 1024 * 1024 // 100MB
	rdb                *redis.Client
	rabbitMQ           *rabbitmq.RabbitMQ
)

func InitBlockService(uploaderURL, pinningServiceURL string, _isDedicatedGateway bool, addr string, amqpConnect string) error {
	if uploaderURL != "" {
		uploader = uploaderURL
	}
	if pinningServiceURL != "" {
		pinningService = pinningServiceURL
	}
	isDedicatedGateway = _isDedicatedGateway

	// Return an error if any of the URLs is empty.
	if uploader == "" || pinningService == "" {
		return errors.New("error: empty url or api key")
	}
	// rdb = redis.NewClusterClient(&redis.ClusterOptions{
	// 	Addrs: addrs,
	// })

	rdb = redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: "", // no password set
		DB:       0,  // use default DB
	})

	rabbitMQ = rabbitmq.InitializeRabbitMQ(amqpConnect, "bandwidth")

	ctx := context.Background()

	rdb.Ping(ctx)
	return nil
}

type Option func(*blockService)

// WriteThrough disable cache checks for writes and make them go straight to
// the blockstore.
func WriteThrough() Option {
	return func(bs *blockService) {
		bs.checkFirst = false
	}
}

// WithAllowlist sets a custom [verifcid.Allowlist] which will be used
func WithAllowlist(allowlist verifcid.Allowlist) Option {
	return func(bs *blockService) {
		bs.allowlist = allowlist
	}
}

// New creates a BlockService with given datastore instance.
func New(bs blockstore.Blockstore, exchange exchange.Interface, opts ...Option) BlockService {
	if exchange == nil {
		logger.Debug("blockservice running in local (offline) mode.")
	}

	service := &blockService{
		allowlist:  verifcid.DefaultAllowlist,
		blockstore: bs,
		exchange:   exchange,
		checkFirst: true,
	}

	for _, opt := range opts {
		opt(service)
	}

	return service
}

// NewWriteThrough creates a BlockService that guarantees writes will go
// through to the blockstore and are not skipped by cache checks.
//
// Deprecated: Use [New] with the [WriteThrough] option.
func NewWriteThrough(bs blockstore.Blockstore, exchange exchange.Interface) BlockService {
	return New(bs, exchange, WriteThrough())
}

// Blockstore returns the blockstore behind this blockservice.
func (s *blockService) Blockstore() blockstore.Blockstore {
	return s.blockstore
}

// Exchange returns the exchange behind this blockservice.
func (s *blockService) Exchange() exchange.Interface {
	return s.exchange
}

func (s *blockService) Allowlist() verifcid.Allowlist {
	return s.allowlist
}

// NewSession creates a new session that allows for
// controlled exchange of wantlists to decrease the bandwidth overhead.
// If the current exchange is a SessionExchange, a new exchange
// session will be created. Otherwise, the current exchange will be used
// directly.
func NewSession(ctx context.Context, bs BlockService) *Session {
	allowlist := verifcid.Allowlist(verifcid.DefaultAllowlist)
	if bbs, ok := bs.(BoundedBlockService); ok {
		allowlist = bbs.Allowlist()
	}
	exch := bs.Exchange()
	if sessEx, ok := exch.(exchange.SessionExchange); ok {
		return &Session{
			allowlist: allowlist,
			sessCtx:   ctx,
			ses:       nil,
			sessEx:    sessEx,
			bs:        bs.Blockstore(),
			notifier:  exch,
		}
	}
	return &Session{
		allowlist: allowlist,
		ses:       exch,
		sessCtx:   ctx,
		bs:        bs.Blockstore(),
		notifier:  exch,
	}
}

// AddBlock adds a particular block to the service, Putting it into the datastore.
func (s *blockService) AddBlock(ctx context.Context, o blocks.Block) error {
	ctx, span := internal.StartSpan(ctx, "blockService.AddBlock")
	defer span.End()

	err := addBlock(ctx, o, s.allowlist)
	if err != nil {
		return err
	}

	if s.exchange != nil {
		if err := s.exchange.NotifyNewBlocks(ctx, o); err != nil {
			logger.Errorf("NotifyNewBlocks: %s", err.Error())
		}
	}

	return nil
}

func addBlock(ctx context.Context, o blocks.Block, allowlist verifcid.Allowlist) error {
	var fr fileRecord
	userID, _ := ctx.Value("userID").(string)
	if userID != "" {
		userKV, err := rdb.Get(ctx, userID).Bytes()
		if err != nil {
			return nil
		} else {
			if err := json.Unmarshal(userKV, &fr); err != nil {
				return err
			}
		}
	}

	c := o.Cid()
	hash := c.Hash().HexString()
	if hash == "122059948439065f29619ef41280cbb932be52c56d99c5966b65e0111239f098bbef" {
		return nil
	}
	// hash security
	if err := verifcid.ValidateCid(allowlist, c); err != nil {
		return err
	}
	var (
		fileRecordID = fr.FileRecordID
		lastSize     uint64
		files        []File
		err          error
		data         []byte
	)

	data, err = rdb.Get(ctx, hash).Bytes()
	if err == nil {
		var f fileInfo

		if err := json.Unmarshal(data, &f); err != nil {
			return nil
		}
	} else if err != redis.Nil {
		return err
	}

	fileRecordID, files, lastSize, err = uploadFiles([]blocks.Block{o}, userID)
	if err != nil {
		return fmt.Errorf("[addBlock] failed to upload file and get file record ID: %w", err)
	}

	if userID != "" {
		f := fileRecord{fileRecordID, lastSize}
		bf, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("failed to marshal `fileInfo`: %w", err)
		}
		if statusCmd := rdb.Set(ctx, userID, []byte(bf), 0); statusCmd.Err() != nil {
			return fmt.Errorf("failed to put data in Redis: %w", statusCmd.Err())
		}
	}
	for _, f := range files {
		if strings.Contains(f.Name, hash) {
			fInfo := fileInfo{fileRecordID, f.CompressedSize64, f.Offset}
			fInfoBytes, err := json.Marshal(fInfo)
			if err != nil {
				return err
			}
			if statusCmd := rdb.Set(ctx, hash, fInfoBytes, 0); statusCmd.Err() != nil {
				return statusCmd.Err()
			}
			break
		}
	}

	logger.Debugf("BlockService.BlockAdded %s", c)

	return nil
}

func uploadFiles(blks []blocks.Block, userID string) (string, []File, uint64, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	// Iterate over files and add them as form parts
	for _, block := range blks {
		// Create a new form file part with the block's hash as the filename
		part, err := writer.CreateFormFile("file", block.Cid().Hash().HexString())
		if err != nil {
			log.Printf("failed to create form file: %v", err)
			return "", nil, 0, err
		}

		// Write the block's raw data to the form file part
		_, err = part.Write(block.RawData())
		if err != nil {
			log.Printf("failed to write data to form file: %v", err)
			return "", nil, 0, err
		}
	}

	// Close the multipart form writer
	if err := writer.Close(); err != nil {
		return "", nil, 0, fmt.Errorf("failed to close multipart writer: %w", err)
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/packUpload", uploader), body)
	if err != nil {
		return "", nil, 0, fmt.Errorf("failed to create HTTP request: %w", err)
	}
	req.Header.Add("Content-Type", writer.FormDataContentType())
	transport := &http.Transport{
		Dial: (&net.Dialer{
			Timeout: 5 * time.Second,
		}).Dial,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	client := http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}

	start := time.Now()

	resp, err := client.Do(req)
	if err != nil {
		return "", nil, 0, fmt.Errorf("failed to post raw data: %w", err)
	}
	defer resp.Body.Close()

	responseTime := time.Since(start)
	if responseTime > 7*time.Second {
		log.Printf("API uploadFiles response time: %v", responseTime)
	}

	type FileRecord struct {
		ID       string `json:"ID"`
		Owner    string `json:"owner"`
		Name     string `json:"name"`
		Size     int    `json:"size"`
		ReaderID int    `json:"readerId"`
	}
	type Response struct {
		FileRecord FileRecord
		ZipReader  *ZipHeader
		ZipHeader  *ZipHeader
	}
	var (
		response     Response
		fileRecordID string
		size         uint64
	)
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
			return "", nil, 0, fmt.Errorf("failed to decode response body: %w", err)
		}
		fileRecordID = response.FileRecord.ID
		if response.ZipHeader == nil && response.ZipReader != nil {
			response.ZipHeader = response.ZipReader
		}

		if response.ZipHeader == nil {
			return "", nil, 0, fmt.Errorf("ZipHeader is nil")
		}

		if len(response.ZipHeader.File) <= len(blks)-1 {
			return "", nil, 0, fmt.Errorf("index out of range")
		}

		size = response.ZipHeader.File[len(blks)-1].Offset + response.ZipHeader.File[len(blks)-1].UncompressedSize64
	} else {
		return "", nil, 0, fmt.Errorf("server returned status %d", resp.StatusCode)
	}
	return fileRecordID, response.ZipHeader.File, size, nil
}

func appendFiles(blks []blocks.Block, fileRecordId string, userID string) ([]File, uint64, error) {
	// Create new multipart form writer
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// Iterate over files and add them as form parts
	for _, block := range blks {
		part, err := writer.CreateFormFile("file", block.Cid().Hash().HexString())
		if err != nil {
			log.Printf("failed to create form file: %v", err)
			return nil, 0, err
		}
		_, err = part.Write(block.RawData())
		if err != nil {
			log.Printf("failed to write data to form file: %v", err)
			return nil, 0, err
		}
	}

	// Close the multipart form writer
	if err := writer.Close(); err != nil {
		return nil, 0, fmt.Errorf("failed to close multipart writer: %w", err)
	}

	// Create new HTTP request and set headers
	req, err := http.NewRequest("POST", fmt.Sprintf("%s/zipAction", uploader), body)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	req.URL.RawQuery = url.Values{
		"file_record_id": {fileRecordId},
		"action_type":    {"1"},
	}.Encode()
	req.Header.Set("Content-Type", writer.FormDataContentType())
	// Send request and handle response
	client := &http.Client{}

	// Create a context that will be cancelled after 1 second
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	start := time.Now()

	req = req.WithContext(ctx)

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	responseTime := time.Since(start)
	log.Printf("API appendFiles response time: %v", responseTime)

	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("server returned status %d, reqURI: %s, file record id: %s", resp.StatusCode, req.URL.String(), fileRecordId)
	}

	var (
		response      ZipHeader
		lastFileIndex int
		lastSize      uint64
	)
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
			return nil, 0, fmt.Errorf("failed to decode response body: %w", err)
		}
		lastFileIndex = len(response.File) - 1

		lastSize = response.File[lastFileIndex].Offset + response.File[lastFileIndex].UncompressedSize64
	} else {
		return nil, 0, fmt.Errorf("server returned status %d", resp.StatusCode)
	}
	return response.File, lastSize, nil
}

func (s *blockService) AddBlocks(ctx context.Context, bs []blocks.Block) error {
	ctx, span := internal.StartSpan(ctx, "blockService.AddBlocks")
	defer span.End()

	toput, err := addBlocks(ctx, bs, s.allowlist)
	if err != nil {
		return err
	}

	if s.exchange != nil {
		logger.Debugf("BlockService.BlockAdded %d blocks", len(toput))
		if err := s.exchange.NotifyNewBlocks(ctx, toput...); err != nil {
			logger.Errorf("NotifyNewBlocks: %s", err.Error())
		}
	}
	return nil
}

func addBlocks(ctx context.Context, bs []blocks.Block, allowlist verifcid.Allowlist) ([]blocks.Block, error) {
	var fr fileRecord
	var toput []blocks.Block

	userID, _ := ctx.Value("userID").(string)
	if userID != "" {
		userKV, err := rdb.Get(ctx, userID).Bytes()
		if err != nil {
			return toput, nil
		} else {
			if err := json.Unmarshal(userKV, &fr); err != nil {
				return nil, err
			}
		}
	}

	// hash security
	for _, b := range bs {
		err := verifcid.ValidateCid(allowlist, b.Cid())
		if err != nil {
			return nil, err
		}
	}

	toput = make([]blocks.Block, 0, len(bs))
	for _, b := range bs {
		hash := b.Cid().Hash().HexString()
		if hash == "122059948439065f29619ef41280cbb932be52c56d99c5966b65e0111239f098bbef" {
			continue
		}
		if err := rdb.Get(ctx, hash).Err(); err == nil {
			continue
		} else {
			toput = append(toput, b)
		}
	}

	if len(toput) == 0 {
		return toput, nil
	}

	var (
		fileRecordID = fr.FileRecordID
		lastSize     uint64
		err          error
		files        []File
	)
	fileRecordID, files, lastSize, err = uploadFiles(toput, userID)
	if err != nil {
		return nil, fmt.Errorf("[addBlocks] failed to upload file and get file record ID: %w", err)
	}

	if userID != "" {
		f := fileRecord{fileRecordID, lastSize}
		bf, err := json.Marshal(f)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal `fileInfo`: %w", err)
		}
		if statusCmd := rdb.Set(ctx, userID, bf, 0); statusCmd.Err() != nil {
			return nil, fmt.Errorf("failed to put data in Redis: %w", statusCmd.Err())
		}
	}

	for _, f := range files {
		for _, b := range toput {
			hash := b.Cid().Hash().HexString()
			if strings.Contains(f.Name, hash) {
				fInfo := fileInfo{fileRecordID, f.CompressedSize64, f.Offset}
				fInfoBytes, err := json.Marshal(fInfo)
				if err != nil {
					return nil, err
				}
				if statusCmd := rdb.Set(ctx, hash, fInfoBytes, 0); statusCmd.Err() != nil {
					return nil, statusCmd.Err()
				}
			}
		}
	}
	return toput, nil
}

// GetBlock retrieves a particular block from the service,
// Getting it from the datastore using the key (hash).
func (s *blockService) GetBlock(ctx context.Context, c cid.Cid) (blocks.Block, error) {
	ctx, span := internal.StartSpan(ctx, "blockService.GetBlock", trace.WithAttributes(attribute.Stringer("CID", c)))
	defer span.End()

	var f func() notifiableFetcher
	if s.exchange != nil {
		f = s.getExchange
	}

	return getBlock(ctx, c, s.blockstore, s.allowlist, f)
}

func (s *blockService) getExchange() notifiableFetcher {
	return s.exchange
}

func getBlock(ctx context.Context, c cid.Cid, bs blockstore.Blockstore, allowlist verifcid.Allowlist, fget func() notifiableFetcher) (blocks.Block, error) {
	err := verifcid.ValidateCid(allowlist, c) // hash security
	if err != nil {
		return nil, err
	}

	kv, err := rdb.Get(ctx, c.Hash().HexString()).Bytes()

	if err == nil {
		var f fileInfo

		if err := json.Unmarshal(kv, &f); err != nil {
			return nil, err
		}

		endpoint, err := url.Parse(fmt.Sprintf("%s/cacheFile/%s", uploader, f.FileRecordID))
		if err != nil {
			return nil, err
		}

		rawQuery := endpoint.Query()
		rawQuery.Set("range", fmt.Sprintf("%d,%d", f.Offset, f.Size))
		endpoint.RawQuery = rawQuery.Encode()
		fileUrl := endpoint.String()

		resp, err := http.Get(fileUrl)
		if err != nil {
			logger.Debugf("Failed to get data %v", err)
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			logger.Debugf("Request failed with status %d", resp.StatusCode)
			return nil, errors.New("failed to get data")
		}

		bdata, err := io.ReadAll(resp.Body)
		if err != nil {
			logger.Debugf("Failed read body %v", err)
			return nil, err
		}
		hash := c.Hash().HexString()

		if err := addBandwidthUsage(isDedicatedGateway, f.Size, hash); err != nil {
			fmt.Printf("Failed to add bandwidth usage: %v", err)
		}

		return blocks.NewBlockWithCid(bdata, c)
	} else {
		fmt.Printf("Hash not found %s, Failed to get data %v \n", c.Hash().HexString(), err)
	}

	if ipld.IsNotFound(err) && fget != nil || err != nil {
		f := fget() // Don't load the exchange until we have to

		// TODO be careful checking ErrNotFound. If the underlying
		// implementation changes, this will break.
		logger.Debug("Blockservice: Searching bitswap")
		blk, err := f.GetBlock(ctx, c)
		if err != nil {
			return nil, err
		}
		if err := addBlock(ctx, blk, allowlist); err != nil {
			return nil, err
		}
		logger.Debugf("BlockService.BlockFetched %s", c)
		return blk, nil
	}

	logger.Debug("BlockService GetBlock: Not found")
	return nil, err
}

// func addBandwidthUsage(isPrivate bool, fileSize uint64, hash string) error {
// 	apiUrl := fmt.Sprintf("%s/api/hourlyUsage/bandwidth/", pinningService)
// 	reqBody, _ := json.Marshal(map[string]interface{}{
// 		"is_private": isPrivate,
// 		"amount":     fileSize,
// 		"cid":        hash,
// 	})
// 	client := &http.Client{}
// 	req, _ := http.NewRequest("POST", apiUrl, bytes.NewBuffer(reqBody))
// 	req.Header.Set("blockservice-API-Key", apiKey)
// 	req.Header.Set("Content-Type", "application/json")
// 	resp, err := client.Do(req)
// 	if err != nil {
// 		logger.Debugf("Failed to send Bandwidth Usage Error %v", err)
// 		return err
// 	}

// 	if resp.StatusCode != http.StatusOK {
// 		log.Printf("Failed to send Bandwidth Usage: %v, isPrivate %v", resp.Status, isPrivate)
// 	}

// 	return nil
// }

func addBandwidthUsage(isPrivate bool, fileSize uint64, hash string) error {
	// reqBody, _ := json.Marshal(map[string]interface{}{
	// 	"is_private": isPrivate,
	// 	"amount":     fileSize,
	// 	"cid":        hash,
	// })

	type AddBandwidthRequest struct {
		IsPrivate bool            `json:"is_private"`
		CID       string          `json:"cid" binding:"required"`
		Amount    decimal.Decimal `gorm:"type:numeric" json:"amount" binding:"required"`
	}

	addBandwidthRequest := AddBandwidthRequest{
		IsPrivate: isPrivate,
		CID:       hash,
		Amount:    decimal.NewFromUint64(fileSize),
	}

	if err := rabbitMQ.Publish(addBandwidthRequest); err != nil {
		log.Printf("Failed to publish message: %v", err)
		return err
	}

	return nil
}

// GetBlocks gets a list of blocks asynchronously and returns through
// the returned channel.
// NB: No guarantees are made about order.
func (s *blockService) GetBlocks(ctx context.Context, ks []cid.Cid) <-chan blocks.Block {
	ctx, span := internal.StartSpan(ctx, "blockService.GetBlocks")
	defer span.End()

	var f func() notifiableFetcher
	if s.exchange != nil {
		f = s.getExchange
	}

	return getBlocks(ctx, ks, s.blockstore, s.allowlist, f)
}

func getBlocks(ctx context.Context, ks []cid.Cid, bs blockstore.Blockstore, allowlist verifcid.Allowlist, fget func() notifiableFetcher) <-chan blocks.Block {
	out := make(chan blocks.Block)

	go func() {
		defer close(out)

		allValid := true
		for _, c := range ks {
			if err := verifcid.ValidateCid(allowlist, c); err != nil {
				allValid = false
				break
			}
		}

		if !allValid {
			// can't shift in place because we don't want to clobber callers.
			ks2 := make([]cid.Cid, 0, len(ks))
			for _, c := range ks {
				// hash security
				if err := verifcid.ValidateCid(allowlist, c); err == nil {
					ks2 = append(ks2, c)
				} else {
					logger.Errorf("unsafe CID (%s) passed to blockService.GetBlocks: %s", c, err)
				}
			}
			ks = ks2
		}

		var misses []cid.Cid
		for _, c := range ks {
			hit, err := getBlockCdn(ctx, c)
			if err != nil {
				misses = append(misses, c)
				continue
			}
			select {
			case out <- hit:
			case <-ctx.Done():
				return
			}
		}

		if len(misses) == 0 || fget == nil {
			return
		}

		f := fget() // don't load exchange unless we have to
		rblocks, err := f.GetBlocks(ctx, misses)
		if err != nil {
			logger.Debugf("Error with GetBlocks: %s", err)
			return
		}

		for {
			var b blocks.Block
			select {
			case v, ok := <-rblocks:
				if !ok {
					return
				}
				b = v
			case <-ctx.Done():
				return
			}

			if err := addBlock(ctx, b, allowlist); err != nil {
				return
			}

			select {
			case out <- b:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func getBlockCdn(ctx context.Context, c cid.Cid) (blocks.Block, error) {
	kv, err := rdb.Get(ctx, c.Hash().HexString()).Bytes()
	if err == nil {
		var f fileInfo

		err := json.Unmarshal(kv, &f)
		if err != nil {
			return nil, err
		}

		endpoint, err := url.Parse(fmt.Sprintf("%s/cacheFile/%s", uploader, f.FileRecordID))
		if err != nil {
			return nil, err
		}

		rawQuery := endpoint.Query()
		rawQuery.Set("range", fmt.Sprintf("%d,%d", f.Offset, f.Size))
		endpoint.RawQuery = rawQuery.Encode()
		fileUrl := endpoint.String()

		resp, err := http.Get(fileUrl)
		if err != nil {
			return nil, err
		}
		hash := c.Hash().HexString()

		if err := addBandwidthUsage(isDedicatedGateway, f.Size, hash); err != nil {
			fmt.Printf("Failed to add bandwidth usage: %v", err)
		}

		bdata, err := io.ReadAll(resp.Body)
		if err == nil {
			return blocks.NewBlockWithCid(bdata, c)
		}
	}
	return nil, err
}

// DeleteBlock deletes a block in the blockservice from the datastore
func (s *blockService) DeleteBlock(ctx context.Context, c cid.Cid) error {
	ctx, span := internal.StartSpan(ctx, "blockService.DeleteBlock", trace.WithAttributes(attribute.Stringer("CID", c)))
	defer span.End()

	err := s.blockstore.DeleteBlock(ctx, c)
	if err == nil {
		logger.Debugf("BlockService.BlockDeleted %s", c)
	}
	return err
}

func (s *blockService) Close() error {
	logger.Debug("blockservice is shutting down...")
	return s.exchange.Close()
}

type notifier interface {
	NotifyNewBlocks(context.Context, ...blocks.Block) error
}

// Session is a helper type to provide higher level access to bitswap sessions
type Session struct {
	allowlist verifcid.Allowlist
	bs        blockstore.Blockstore
	ses       exchange.Fetcher
	sessEx    exchange.SessionExchange
	sessCtx   context.Context
	notifier  notifier
	lk        sync.Mutex
}

type notifiableFetcher interface {
	exchange.Fetcher
	notifier
}

type notifiableFetcherWrapper struct {
	exchange.Fetcher
	notifier
}

func (s *Session) getSession() notifiableFetcher {
	s.lk.Lock()
	defer s.lk.Unlock()
	if s.ses == nil {
		s.ses = s.sessEx.NewSession(s.sessCtx)
	}

	return notifiableFetcherWrapper{s.ses, s.notifier}
}

func (s *Session) getExchange() notifiableFetcher {
	return notifiableFetcherWrapper{s.ses, s.notifier}
}

func (s *Session) getFetcherFactory() func() notifiableFetcher {
	if s.sessEx != nil {
		return s.getSession
	}
	if s.ses != nil {
		// Our exchange isn't session compatible, let's fallback to non sessions fetches
		return s.getExchange
	}
	return nil
}

// GetBlock gets a block in the context of a request session
func (s *Session) GetBlock(ctx context.Context, c cid.Cid) (blocks.Block, error) {
	ctx, span := internal.StartSpan(ctx, "Session.GetBlock", trace.WithAttributes(attribute.Stringer("CID", c)))
	defer span.End()

	return getBlock(ctx, c, s.bs, s.allowlist, s.getFetcherFactory())
}

// GetBlocks gets blocks in the context of a request session
func (s *Session) GetBlocks(ctx context.Context, ks []cid.Cid) <-chan blocks.Block {
	ctx, span := internal.StartSpan(ctx, "Session.GetBlocks")
	defer span.End()

	return getBlocks(ctx, ks, s.bs, s.allowlist, s.getFetcherFactory())
}

var _ BlockGetter = (*Session)(nil)
