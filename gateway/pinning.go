package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/shopspring/decimal"
	"golang.org/x/time/rate"
	"net/http"
	"strings"
	"time"
)

func (i *handler) checkDmca(ctx context.Context, cid string) error {
	type CheckDmcaRequest struct {
		Hash string `json:"hash"`
	}
	checkDmcaRequest := &CheckDmcaRequest{
		Hash: cid,
	}
	payload, err := json.Marshal(checkDmcaRequest)
	if err != nil {
		return err
	}
	checkDmcaEndpoint := fmt.Sprintf("%s/api/content/dmca/check", i.pinningApiEndpoint)
	req, err := http.NewRequestWithContext(ctx, "POST", checkDmcaEndpoint, bytes.NewBuffer(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("blockservice-API-Key", i.blockServiceApiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	type CheckDmcaResponse struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	var response CheckDmcaResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return err
	}
	return errors.New(response.Message)
}

func (i *handler) checkHashStatus(ctx context.Context, r *http.Request, rootCid string) error {
	type GetHashStatusRequest struct {
		Hash      string `json:"hash"`
		Subdomain string `json:"subdomain"`
	}
	// api.example.com, domain: example.com => subdomain 'api'
	subdomain := strings.TrimSuffix(r.Host, fmt.Sprintf(".%s", i.domain))

	checkHashStatusRequest := &GetHashStatusRequest{
		Hash:      rootCid,
		Subdomain: subdomain,
	}
	payload, err := json.Marshal(checkHashStatusRequest)
	if err != nil {
		return err
	}
	checkHashStatusEndpoint := fmt.Sprintf("%s/api/content/hash/check", i.pinningApiEndpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, checkHashStatusEndpoint, bytes.NewBuffer(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("blockservice-API-Key", i.blockServiceApiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	type GetHashStatusResponse struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	var response GetHashStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return err
	}
	return errors.New(response.Message)
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

func (i *handler) addFileDownloadRequest(r *http.Request, rootCid string, fileSize uint64, isPremium bool, isSuccess bool) {
	type AddFileDownloadRequest struct {
		CID       string    `json:"cid"`
		Gateway   string    `json:"gateway"`
		Success   bool      `json:"success"`
		FileSize  uint64    `json:"file_size"`
		IsPremium bool      `json:"is_premium"`
		Timestamp time.Time `json:"timestamp"`
	}
	subdomain := strings.TrimSuffix(r.Host, fmt.Sprintf(".%s", i.domain))
	addFileDownloadRequest := &AddFileDownloadRequest{
		CID:       rootCid,
		Gateway:   subdomain,
		Success:   isSuccess,
		FileSize:  fileSize,
		IsPremium: isPremium,
		Timestamp: time.Now(),
	}

	if err := i.fileDownloadRequestRabbitMQ.Publish(addFileDownloadRequest); err != nil {
		log.Errorf("Failed to publish AddFileDownloadRequest: %v", err)
	}
}

func (i *handler) addBandwidthUsage(r *http.Request, rootCid string, fileSize uint64, isPremium bool) {
	type AddBandwidthRequest struct {
		CID       string          `json:"cid" binding:"required"`
		Gateway   string          `json:"gateway" binding:"required"`
		Amount    decimal.Decimal `gorm:"type:numeric" json:"amount" binding:"required"`
		IsPremium bool            `json:"isPremium" binding:"required"`
	}

	// api.example.com, domain: example.com => subdomain 'api'
	subdomain := strings.TrimSuffix(r.Host, fmt.Sprintf(".%s", i.domain))
	addBandwidthRequest := AddBandwidthRequest{
		CID:       rootCid,
		Amount:    decimal.NewFromUint64(fileSize),
		Gateway:   subdomain,
		IsPremium: isPremium,
	}

	if err := i.rabbitMQ.Publish(addBandwidthRequest); err != nil {
		log.Errorf("failed to publish AddBandwidthRequest: %s", err.Error())
	}
}

func (i *handler) checkRateLimit(isPremium bool, cid string, ip string) error {
	if isPremium {
		return nil
	}
	ipLimiter := i.getLimiter(ip, i.ipLimiter, 100)
	if !ipLimiter.Allow() {
		return errors.New("Too many requests from this IP")
	}
	cidLimiter := i.getLimiter(cid, i.ipLimiter, 15)
	if !cidLimiter.Allow() {
		return errors.New("Too many requests for this CID")
	}
	return nil
}

func (i *handler) getLimiter(limit string, limitMap map[string]*rate.Limiter, rps float64) *rate.Limiter {
	i.lock.Lock()
	defer i.lock.Unlock()

	limiter, exists := limitMap[limit]
	if !exists {
		limiter = rate.NewLimiter(rate.Every(time.Minute), int(rps))
		limitMap[limit] = limiter
	}

	return limiter
}
