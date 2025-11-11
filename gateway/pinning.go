package gateway

import (
	"time"
)

func (i *handler) addFileDownloadRequest(rootCid string, fileSize uint64, isSuccess bool) {
	type AddFileDownloadRequest struct {
		CID       string    `json:"cid"`
		Success   bool      `json:"success"`
		FileSize  uint64    `json:"file_size"`
		IsPremium bool      `json:"is_premium"`
		Timestamp time.Time `json:"timestamp"`
	}

	addFileDownloadRequest := &AddFileDownloadRequest{
		CID:       rootCid,
		Success:   isSuccess,
		FileSize:  fileSize,
		IsPremium: i.isDedicatedGateway,
		Timestamp: time.Now(),
	}

	if err := i.fileDownloadRequestRabbitMQ.Publish(addFileDownloadRequest); err != nil {
		log.Errorf("Failed to publish AddFileDownloadRequest: %v", err)
	}
}
