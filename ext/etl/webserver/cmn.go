// Package webserver provides a framework to impelemnt etl transformation webserver in golang.
/*
 * Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.
 */
package webserver

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn/cos"
)

const GetContentType = "binary/octet-stream"

func setResponseHeaders(header http.Header, size int64) {
	if size > 0 {
		header.Set(cos.HdrContentLength, strconv.FormatInt(size, 10))
	}
	header.Set(cos.HdrContentType, GetContentType)
}

// Returns an error with message if status code was > 200
func wrapHTTPError(resp *http.Response, err error) (*http.Response, error) {
	if err != nil {
		return resp, err
	}

	if resp.StatusCode > http.StatusNoContent {
		if resp.Body == nil {
			return resp, errors.New(resp.Status)
		}
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return resp, err
		}
		return resp, fmt.Errorf("%s %s", resp.Status, string(b))
	}

	return resp, nil
}

func parsePipelineURL(pipelineURLHdr string) (string, string) {
	pipelineURLHdr = strings.TrimSpace(pipelineURLHdr)
	if pipelineURLHdr == "" {
		return "", ""
	}
	pipelineURLs := strings.Split(pipelineURLHdr, apc.ETLPipelineSeparator)
	for _, pipelineURL := range pipelineURLs {
		pipelineURL = strings.TrimSpace(pipelineURL)
		if pipelineURL == "" {
			return "", ""
		}
	}
	commaIndex := strings.Index(pipelineURLHdr, apc.ETLPipelineSeparator)
	if commaIndex == -1 {
		// No comma found, only one URL (already validated)
		return pipelineURLHdr, ""
	}
	// Extract first URL and remaining pipeline
	return pipelineURLHdr[:commaIndex], pipelineURLHdr[commaIndex+1:]
}
