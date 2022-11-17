// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2022, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/dloader"
	jsoniter "github.com/json-iterator/go"
)

// [METHOD] /v1/download
func (p *proxy) downloadHandler(w http.ResponseWriter, r *http.Request) {
	if !p.ClusterStarted() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodDelete:
		p.httpdladm(w, r)
	case http.MethodPost:
		p.httpdlpost(w, r)
	default:
		cmn.WriteErr405(w, r, http.MethodDelete, http.MethodGet, http.MethodPost)
	}
}

// httpDownloadAdmin is meant for aborting, removing and getting status updates for downloads.
// GET /v1/download?id=...
// DELETE /v1/download/{abort, remove}?id=...
func (p *proxy) httpdladm(w http.ResponseWriter, r *http.Request) {
	payload := &dloader.AdminBody{}
	if !p.ClusterStarted() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if err := cmn.ReadJSON(w, r, &payload); err != nil {
		return
	}
	if err := payload.Validate(r.Method == http.MethodDelete); err != nil {
		p.writeErr(w, r, err)
		return
	}

	if r.Method == http.MethodDelete {
		items, err := cmn.MatchItems(r.URL.Path, 1, false, apc.URLPathDownload.L)
		if err != nil {
			p.writeErr(w, r, err)
			return
		}

		if items[0] != apc.Abort && items[0] != apc.Remove {
			p.writeErrAct(w, r, items[0])
			return
		}
	}

	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("httpDownloadAdmin payload %v", payload)
	}
	if payload.ID != "" && p.ic.redirectToIC(w, r) {
		return
	}
	resp, statusCode, err := p.dladm(r.Method, r.URL.Path, payload)
	if err != nil {
		p.writeErr(w, r, err, statusCode)
		return
	}
	w.Write(resp)
}

// POST /v1/download
func (p *proxy) httpdlpost(w http.ResponseWriter, r *http.Request) {
	var (
		body             []byte
		dlb              dloader.Body
		dlBase           dloader.Base
		err              error
		ok               bool
		progressInterval = dloader.DownloadProgressInterval
	)

	if _, err = p.apiItems(w, r, 0, false, apc.URLPathDownload.L); err != nil {
		return
	}

	if body, err = io.ReadAll(r.Body); err != nil {
		p.writeErrStatusf(w, r, http.StatusInternalServerError, "Error starting download: %v", err.Error())
		return
	}

	if dlb, dlBase, ok = p.validateStartDownload(w, r, body); !ok {
		return
	}

	if dlBase.ProgressInterval != "" {
		if progressInterval, err = time.ParseDuration(dlBase.ProgressInterval); err != nil {
			p.writeErrf(w, r, "%s: invalid progress interval %q: %v", p, dlBase.ProgressInterval, err)
			return
		}
	}

	// const prefix to visually differentiate xactions and dl. jobs
	jobID := "dlj-" + cos.GenUUID()

	if errCode, err := p.dlstart(r, jobID, body); err != nil {
		p.writeErrStatusf(w, r, errCode, "Error starting download: %v", err)
		return
	}
	smap := p.owner.smap.get()
	nl := dloader.NewDownloadNL(jobID, string(dlb.Type), &smap.Smap, progressInterval)
	nl.SetOwner(equalIC)
	p.ic.registerEqual(regIC{nl: nl, smap: smap})

	_respWithID(w, jobID)
}

func (p *proxy) dladm(method, path string, msg *dloader.AdminBody) ([]byte, int, error) {
	var (
		notFoundCnt int
		err         error
	)
	if msg.ID != "" && method == http.MethodGet && msg.OnlyActive {
		if stats, exists := p.notifs.queryStats(msg.ID); exists {
			var resp *dloader.StatusResp
			stats.Range(func(_ string, status any) bool {
				var (
					dlStatus *dloader.StatusResp
					ok       bool
				)
				if dlStatus, ok = status.(*dloader.StatusResp); !ok {
					dlStatus = &dloader.StatusResp{}
					if err := cos.MorphMarshal(status, dlStatus); err != nil {
						debug.AssertNoErr(err)
						return false
					}
				}
				resp = resp.Aggregate(*dlStatus)
				return true
			})

			respJSON := cos.MustMarshal(resp)
			return respJSON, http.StatusOK, nil
		}
	}

	var (
		config = cmn.GCO.Get()
		body   = cos.MustMarshal(msg)
		args   = allocBcArgs()
	)
	args.req = cmn.HreqArgs{Method: method, Path: path, Body: body, Query: url.Values{}}
	args.timeout = config.Timeout.MaxHostBusy.D()
	results := p.bcastGroup(args)
	defer freeBcastRes(results)
	freeBcArgs(args)
	respCnt := len(results)

	if respCnt == 0 {
		return nil, http.StatusBadRequest, cmn.NewErrNoNodes(apc.Target)
	}
	validResponses := make([]*callResult, 0, respCnt) // TODO: avoid allocation
	for _, res := range results {
		if res.status == http.StatusOK {
			validResponses = append(validResponses, res)
			continue
		}
		if res.status != http.StatusNotFound {
			return nil, res.status, res.err
		}
		notFoundCnt++
		err = res.err
	}
	if notFoundCnt == respCnt { // All responded with 404.
		return nil, http.StatusNotFound, err
	}

	switch method {
	case http.MethodGet:
		if msg.ID == "" {
			// If ID is empty, return the list of downloads
			aggregate := make(map[string]*dloader.Job)
			for _, resp := range validResponses {
				if len(resp.bytes) == 0 {
					continue
				}
				var parsedResp map[string]*dloader.Job
				if err := jsoniter.Unmarshal(resp.bytes, &parsedResp); err != nil {
					return nil, http.StatusInternalServerError, err
				}
				for k, v := range parsedResp {
					if oldMetric, ok := aggregate[k]; ok {
						v.Aggregate(oldMetric)
					}
					aggregate[k] = v
				}
			}

			listDownloads := make(dloader.JobInfos, 0, len(aggregate))
			for _, v := range aggregate {
				listDownloads = append(listDownloads, v)
			}
			result := cos.MustMarshal(listDownloads)
			return result, http.StatusOK, nil
		}

		var stResp *dloader.StatusResp
		for _, resp := range validResponses {
			status := dloader.StatusResp{}
			if err := jsoniter.Unmarshal(resp.bytes, &status); err != nil {
				return nil, http.StatusInternalServerError, err
			}
			stResp = stResp.Aggregate(status)
		}
		body := cos.MustMarshal(stResp)
		return body, http.StatusOK, nil
	case http.MethodDelete:
		res := validResponses[0]
		return res.bytes, res.status, res.err
	default:
		debug.Assert(false, method)
		return nil, http.StatusInternalServerError, nil
	}
}

func (p *proxy) dlstart(r *http.Request, id string, body []byte) (errCode int, err error) {
	query := r.URL.Query()
	query.Set(apc.QparamUUID, id)
	args := allocBcArgs()
	args.req = cmn.HreqArgs{Method: http.MethodPost, Path: r.URL.Path, Body: body, Query: query}
	config := cmn.GCO.Get()
	args.timeout = config.Timeout.MaxHostBusy.D()
	results := p.bcastGroup(args)
	defer freeBcastRes(results)
	freeBcArgs(args)
	for _, res := range results {
		if res.err == nil {
			continue
		}
		errCode, err = res.status, res.err
		return
	}
	return http.StatusOK, nil
}

func (p *proxy) validateStartDownload(w http.ResponseWriter, r *http.Request, body []byte) (dlb dloader.Body, dlBase dloader.Base, ok bool) {
	if err := jsoniter.Unmarshal(body, &dlb); err != nil {
		err = fmt.Errorf(cmn.FmtErrUnmarshal, p, "download request", cos.BHead(body), err)
		p.writeErr(w, r, err)
		return
	}
	if err := jsoniter.Unmarshal(dlb.RawMessage, &dlBase); err != nil {
		err = fmt.Errorf(cmn.FmtErrUnmarshal, p, "download message", cos.BHead(dlb.RawMessage), err)
		p.writeErr(w, r, err)
		return
	}
	bck := cluster.CloneBck(&dlBase.Bck)
	args := bckInitArgs{p: p, w: w, r: r, reqBody: body, bck: bck, perms: apc.AccessRW}
	args.createAIS = true
	if _, err := args.initAndTry(); err == nil {
		ok = true
	}
	return
}

func _respWithID(w http.ResponseWriter, id string) {
	w.Header().Set(cos.HdrContentType, cos.ContentJSON)
	b := cos.MustMarshal(dloader.DlPostResp{ID: id})
	_, err := w.Write(b)
	debug.AssertNoErr(err)
}
