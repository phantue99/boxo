// Package blockservice implements a BlockService interface that provides
// a single GetBlock/AddBlock interface that seamlessly retrieves data either
// locally or from a remote peer through the exchange.
package blockservice

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	aiozimageoptimizer "github.com/lamgiahungaioz/aioz-image-optimizer"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"
	logging "github.com/ipfs/go-log/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/ipfs/boxo/blockservice/internal"
	"github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/exchange"
	"github.com/ipfs/boxo/rabbitmq"
	"github.com/ipfs/boxo/verifcid"
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
	uploader             string
	pinningService       string
	isDedicatedGateway   bool
	maxSize              = 100 * 1024 * 1024 // 100MB
	rabbitMQ             *rabbitmq.RabbitMQ
	defaultChunkHash     = "122059948439065f29619ef41280cbb932be52c56d99c5966b65e0111239f098bbef"
	blockEncryptionKey   string
	encryptedBlockPrefix string
	blockServiceApiKey   string
)

func InitBlockService(uploaderURL, pinningServiceURL string, _isDedicatedGateway bool, blockServiceKey string, amqpConnect string, encryptionKey string, encryptedBlockDataPrefix string) error {
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

	rabbitMQ = rabbitmq.InitializeRabbitMQ(amqpConnect, "bandwidth")
	blockEncryptionKey = encryptionKey
	encryptedBlockPrefix = encryptedBlockDataPrefix
	blockServiceApiKey = blockServiceKey

	aiozimageoptimizer.SetExiftoolBinPath("/usr/bin/exiftool")
	aiozimageoptimizer.SetFfmpegBinPath("/usr/bin/ffmpeg")

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
	c := o.Cid()
	hash := c.Hash().HexString()
	if hash == defaultChunkHash {
		return nil
	}
	// hash security
	if err := verifcid.ValidateCid(allowlist, c); err != nil {
		return err
	}
	var (
		fileRecordID string
		files        []File
		err          error
	)

	var f fileInfo
	if err := getKey(ctx, hash, &f); err != nil && f.FileRecordID != "" {
		return nil
	}

	fileRecordID, files, err = uploadFiles([]blocks.Block{o})
	if err != nil {
		return fmt.Errorf("[addBlock] failed to upload file and get file record ID: %w", err)
	}

	for _, f := range files {
		if strings.Contains(f.Name, hash) {
			fInfo := fileInfo{fileRecordID, f.CompressedSize64, f.Offset}
			if err := setKey(ctx, hash, fInfo, -1); err != nil {
				return err
			}
		}
	}

	logger.Debugf("BlockService.BlockAdded %s", c)

	return nil
}

func uploadFiles(blks []blocks.Block) (string, []File, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	// Iterate over files and add them as form parts
	for _, block := range blks {
		// Create a new form file part with the block's hash as the filename
		part, err := writer.CreateFormFile("file", block.Cid().Hash().HexString())
		if err != nil {
			log.Printf("failed to create form file: %v", err)
			return "", nil, err
		}

		// gen 32-byte key from master key + cid hash
		cidHash := block.Cid().Hash().HexString()
		encryptKey := sha256.Sum256([]byte(cidHash + blockEncryptionKey))
		cipherBlock, err := aes.NewCipher(encryptKey[:])
		if err != nil {
			return "", nil, err
		}
		gcm, err := cipher.NewGCM(cipherBlock)
		if err != nil {
			return "", nil, err
		}
		nonce := make([]byte, gcm.NonceSize())
		if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
			return "", nil, err
		}
		// Encrypt the block's raw data
		encryptedData := gcm.Seal(nonce, nonce, block.RawData(), nil)
		// Prepend prefix to encrypted data
		encryptedData = append([]byte(encryptedBlockPrefix), encryptedData...)

		// Write the block's encrypted data to the form file part
		_, err = part.Write(encryptedData)
		if err != nil {
			log.Printf("failed to write data to form file: %v", err)
			return "", nil, err
		}
	}

	// Close the multipart form writer
	if err := writer.Close(); err != nil {
		return "", nil, fmt.Errorf("failed to close multipart writer: %w", err)
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/packUpload", uploader), body)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create HTTP request: %w", err)
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
		return "", nil, fmt.Errorf("failed to post raw data: %w", err)
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
	)
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
			return "", nil, fmt.Errorf("failed to decode response body: %w", err)
		}
		fileRecordID = response.FileRecord.ID
		if response.ZipHeader == nil && response.ZipReader != nil {
			response.ZipHeader = response.ZipReader
		}

		if response.ZipHeader == nil {
			return "", nil, fmt.Errorf("ZipHeader is nil")
		}

		if len(response.ZipHeader.File) <= len(blks)-1 {
			return "", nil, fmt.Errorf("index out of range")
		}
	} else {
		return "", nil, fmt.Errorf("server returned status %d", resp.StatusCode)
	}
	return fileRecordID, response.ZipHeader.File, nil
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
	var toput []blocks.Block

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
		if hash == defaultChunkHash {
			continue
		}
		var _existenceCheck any
		if err := getKey(ctx, hash, &_existenceCheck); err == nil && _existenceCheck != nil {
			continue
		} else {
			toput = append(toput, b)
		}
	}

	if len(toput) == 0 {
		return toput, nil
	}

	var (
		fileRecordID string
		err          error
		files        []File
	)
	fileRecordID, files, err = uploadFiles(toput)
	if err != nil {
		return nil, fmt.Errorf("[addBlocks] failed to upload file and get file record ID: %w", err)
	}

	for _, f := range files {
		for _, b := range toput {
			hash := b.Cid().Hash().HexString()
			if strings.Contains(f.Name, hash) {
				fInfo := fileInfo{fileRecordID, f.CompressedSize64, f.Offset}
				if err := setKey(ctx, hash, fInfo, -1); err != nil {
					return nil, err
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

	var f fileInfo
	if err := getKey(ctx, c.Hash().HexString(), &f); err == nil && f.FileRecordID != "" {
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
		isEncryptedBlock := bytes.HasPrefix(bdata, []byte(encryptedBlockPrefix))
		defer func() {
			if isEncryptedBlock {
				return
			}
			// delete block on cdn
			if err := deleteBlockCdn(f.FileRecordID); err != nil {
				logger.Debugf("Failed delete unencrypted block: %v", err)
				return
			}
			// get block based on data + cid and upload to cdn
			blk, err := blocks.NewBlockWithCid(bdata, c)
			if err != nil {
				return
			}
			if err := addBlock(ctx, blk, allowlist); err != nil {
				return
			}
		}()
		if isEncryptedBlock {
			blkEncryptKey := sha256.Sum256([]byte(hash + blockEncryptionKey))
			cipherBlock, err := aes.NewCipher(blkEncryptKey[:])
			if err != nil {
				return nil, errors.New("malformed block encryption key")
			}
			gcm, err := cipher.NewGCM(cipherBlock)
			if err != nil {
				return nil, errors.New("malformed block encryption key")
			}
			nonceSize := gcm.NonceSize()
			if len(bdata) < nonceSize {
				return nil, errors.New("encrypted block nonce size is invalid")
			}
			// removes the encryption prefix
			bdata = bdata[len(encryptedBlockPrefix):]

			nonce, cipherText := bdata[:nonceSize], bdata[nonceSize:]
			bdata, err = gcm.Open(nil, nonce, cipherText, nil)
			if err != nil {
				return nil, err
			}
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
			hit, err := getBlockCdn(ctx, c, allowlist)
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

func getBlockCdn(ctx context.Context, c cid.Cid, allowlist verifcid.Allowlist) (blocks.Block, error) {
	var f fileInfo
	if err := getKey(ctx, c.Hash().String(), &f); err != nil {
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

	bdata, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	isEncryptedBlock := bytes.HasPrefix(bdata, []byte(encryptedBlockPrefix))
	defer func() {
		if isEncryptedBlock {
			return
		}
		// delete block on cdn
		if err := deleteBlockCdn(f.FileRecordID); err != nil {
			logger.Debugf("Failed delete unencrypted block: %v", err)
			return
		}
		// get block based on data + cid and upload to cdn
		blk, err := blocks.NewBlockWithCid(bdata, c)
		if err != nil {
			return
		}
		if err := addBlock(ctx, blk, allowlist); err != nil {
			return
		}
	}()
	if isEncryptedBlock {
		blkEncryptKey := sha256.Sum256([]byte(hash + blockEncryptionKey))
		cipherBlock, err := aes.NewCipher(blkEncryptKey[:])
		if err != nil {
			return nil, errors.New("malformed block encryption key")
		}
		gcm, err := cipher.NewGCM(cipherBlock)
		if err != nil {
			return nil, errors.New("malformed block encryption key")
		}
		nonceSize := gcm.NonceSize()
		if len(bdata) < nonceSize {
			return nil, errors.New("encrypted block nonce size is invalid")
		}
		// removes the encryption prefix
		bdata = bdata[len(encryptedBlockPrefix):]

		nonce, cipherText := bdata[:nonceSize], bdata[nonceSize:]
		bdata, err = gcm.Open(nil, nonce, cipherText, nil)
		if err != nil {
			return nil, err
		}
	}
	return blocks.NewBlockWithCid(bdata, c)
}

func deleteBlockCdn(fileRecordId string) error {
	url := fmt.Sprintf("%s/endFileRecord?file_record_id=%s", uploader, fileRecordId)
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return err
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("request delete block failed")
	}

	return nil
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

func getKey(ctx context.Context, key string, out interface{}) error {
	getKeyEndpoint := fmt.Sprintf("%s/api/key-val?key=%s", pinningService, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, getKeyEndpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("blockservice-API-Key", blockServiceApiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		type errResponse struct {
			Error string `json:"error"`
		}
		var errResp errResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
			return err
		}
		return errors.New(errResp.Error)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func setKey(ctx context.Context, key string, value any, expiration int64) error {
	type SetKeyRequest struct {
		Key        string      `json:"key"`
		Value      interface{} `json:"value"`
		Expiration int64       `json:"expiration"`
	}

	setKeyEndpoint := fmt.Sprintf("%s/api/key-val", pinningService)
	reqData, err := json.Marshal(SetKeyRequest{key, value, expiration})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, setKeyEndpoint, bytes.NewBuffer(reqData))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("blockservice-API-Key", blockServiceApiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("set key failed: %s", resp.Status)
	}
	return nil
}
