package scanner

import (
	common "bitbucket.org/bdsengineering/perceptor/pkg/common"
)

type ScanClientInterface interface {
	Scan(job ScanJob) (*ScanClientJobResults, error)
	ScanCliSh(job ScanJob) error
	ScanDockerSh(job ScanJob) error
}

type ScanJob struct {
	Image common.Image
}

func NewScanJob(image common.Image) *ScanJob {
	return &ScanJob{Image: image}
}
