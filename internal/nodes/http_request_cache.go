package nodes

import (
	"bytes"
	"errors"
	"github.com/TeaOSLab/EdgeCommon/pkg/rpc/pb"
	"github.com/TeaOSLab/EdgeNode/internal/caches"
	"github.com/TeaOSLab/EdgeNode/internal/goman"
	"github.com/TeaOSLab/EdgeNode/internal/remotelogs"
	"github.com/TeaOSLab/EdgeNode/internal/rpc"
	"github.com/TeaOSLab/EdgeNode/internal/utils"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 读取缓存
func (this *HTTPRequest) doCacheRead(useStale bool) (shouldStop bool) {
	this.cacheCanTryStale = false

	var cachePolicy = this.ReqServer.HTTPCachePolicy
	if cachePolicy == nil || !cachePolicy.IsOn {
		return
	}

	if this.web.Cache == nil || !this.web.Cache.IsOn || (len(cachePolicy.CacheRefs) == 0 && len(this.web.Cache.CacheRefs) == 0) {
		return
	}

	// 判断是否在预热
	if (strings.HasPrefix(this.RawReq.RemoteAddr, "127.") || strings.HasPrefix(this.RawReq.RemoteAddr, "[::1]")) && this.RawReq.Header.Get("X-Cache-Action") == "preheat" {
		return
	}

	var addStatusHeader = this.web.Cache.AddStatusHeader
	if addStatusHeader {
		defer func() {
			cacheStatus := this.varMapping["cache.status"]
			if cacheStatus != "HIT" {
				this.writer.Header().Set("X-Cache", cacheStatus)
			}
		}()
	}

	// 检查服务独立的缓存条件
	refType := ""
	for _, cacheRef := range this.web.Cache.CacheRefs {
		if !cacheRef.IsOn ||
			cacheRef.Conds == nil ||
			!cacheRef.Conds.HasRequestConds() {
			continue
		}
		if cacheRef.Conds.MatchRequest(this.Format) {
			if cacheRef.IsReverse {
				return
			}
			this.cacheRef = cacheRef
			refType = "server"
			break
		}
	}
	if this.cacheRef == nil {
		// 检查策略默认的缓存条件
		for _, cacheRef := range cachePolicy.CacheRefs {
			if !cacheRef.IsOn ||
				cacheRef.Conds == nil ||
				!cacheRef.Conds.HasRequestConds() {
				continue
			}
			if cacheRef.Conds.MatchRequest(this.Format) {
				if cacheRef.IsReverse {
					return
				}
				this.cacheRef = cacheRef
				refType = "policy"
				break
			}
		}

		if this.cacheRef == nil {
			return
		}
	}

	// 校验请求
	if !this.cacheRef.MatchRequest(this.RawReq) {
		this.cacheRef = nil
		return
	}

	// 相关变量
	this.varMapping["cache.policy.name"] = cachePolicy.Name
	this.varMapping["cache.policy.id"] = strconv.FormatInt(cachePolicy.Id, 10)
	this.varMapping["cache.policy.type"] = cachePolicy.Type

	// Cache-Pragma
	if this.cacheRef.EnableRequestCachePragma {
		if this.RawReq.Header.Get("Cache-Control") == "no-cache" || this.RawReq.Header.Get("Pragma") == "no-cache" {
			this.cacheRef = nil
			return
		}
	}

	// TODO 支持Vary Header

	// 检查是否有缓存
	key := this.Format(this.cacheRef.Key)
	if len(key) == 0 {
		this.cacheRef = nil
		return
	}

	this.cacheKey = key
	this.varMapping["cache.key"] = key

	// 读取缓存
	storage := caches.SharedManager.FindStorageWithPolicy(cachePolicy.Id)
	if storage == nil {
		this.cacheRef = nil
		return
	}

	// 判断是否在Purge
	if this.web.Cache.PurgeIsOn && strings.ToUpper(this.RawReq.Method) == "PURGE" && this.RawReq.Header.Get("X-Edge-Purge-Key") == this.web.Cache.PurgeKey {
		this.varMapping["cache.status"] = "PURGE"

		err := storage.Delete(key)
		if err != nil {
			remotelogs.Error("HTTP_REQUEST_CACHE", "purge failed: "+err.Error())
		}

		goman.New(func() {
			rpcClient, err := rpc.SharedRPC()
			if err == nil {
				for _, rpcServerService := range rpcClient.ServerRPCList() {
					_, err = rpcServerService.PurgeServerCache(rpcClient.Context(), &pb.PurgeServerCacheRequest{
						Domains:  []string{this.ReqHost},
						Keys:     []string{key},
						Prefixes: nil,
					})
					if err != nil {
						remotelogs.Error("HTTP_REQUEST_CACHE", "purge failed: "+err.Error())
					}
				}
			}
		})

		return true
	}

	// 调用回调
	this.onRequest()
	if this.writer.isFinished {
		return
	}

	var reader caches.Reader
	var err error

	// 是否优先检查WebP
	var isWebP = false
	if this.web.WebP != nil &&
		this.web.WebP.IsOn &&
		this.web.WebP.MatchRequest(filepath.Ext(this.Path()), this.Format) &&
		this.web.WebP.MatchAccept(this.requestHeader("Accept")) {
		reader, _ = storage.OpenReader(key+webpSuffix, useStale)
		if reader != nil {
			isWebP = true
		}
	}

	// 检查正常的文件
	if reader == nil {
		reader, err = storage.OpenReader(key, useStale)
		if err != nil {
			if err == caches.ErrNotFound {
				// cache相关变量
				this.varMapping["cache.status"] = "MISS"

				if !useStale && this.web.Cache.Stale != nil && this.web.Cache.Stale.IsOn {
					this.cacheCanTryStale = true
				}
				return
			}

			if !this.canIgnore(err) {
				remotelogs.Warn("HTTP_REQUEST_CACHE", this.URL()+": read from cache failed: open cache failed: "+err.Error())
			}
			return
		}
	}

	defer func() {
		if !this.writer.DelayRead() {
			_ = reader.Close()
		}
	}()

	if useStale {
		this.varMapping["cache.status"] = "STALE"
		this.logAttrs["cache.status"] = "STALE"
	} else {
		this.varMapping["cache.status"] = "HIT"
		this.logAttrs["cache.status"] = "HIT"
	}

	// 准备Buffer
	var pool = this.bytePool(reader.BodySize())
	var buf = pool.Get()
	defer func() {
		pool.Put(buf)
	}()

	// 读取Header
	var headerBuf = []byte{}
	err = reader.ReadHeader(buf, func(n int) (goNext bool, err error) {
		headerBuf = append(headerBuf, buf[:n]...)
		for {
			nIndex := bytes.Index(headerBuf, []byte{'\n'})
			if nIndex >= 0 {
				row := headerBuf[:nIndex]
				spaceIndex := bytes.Index(row, []byte{':'})
				if spaceIndex <= 0 {
					return false, errors.New("invalid header '" + string(row) + "'")
				}

				this.writer.Header().Set(string(row[:spaceIndex]), string(row[spaceIndex+1:]))
				headerBuf = headerBuf[nIndex+1:]
			} else {
				break
			}
		}
		return true, nil
	})
	if err != nil {
		if !this.canIgnore(err) {
			remotelogs.Warn("HTTP_REQUEST_CACHE", this.URL()+": read from cache failed: read header failed: "+err.Error())
		}
		return
	}

	// 设置cache.age变量
	var age = strconv.FormatInt(reader.ExpiresAt()-utils.UnixTime(), 10)
	this.varMapping["cache.age"] = age

	if addStatusHeader {
		if useStale {
			this.writer.Header().Set("X-Cache", "STALE, "+refType+", "+reader.TypeName())
		} else {
			this.writer.Header().Set("X-Cache", "HIT, "+refType+", "+reader.TypeName())
		}
	}
	if this.web.Cache.AddAgeHeader {
		this.writer.Header().Set("Age", age)
	}

	// ETag
	// 这里强制设置ETag，如果先前源站设置了ETag，将会被覆盖，避免因为源站的ETag导致源站返回304 Not Modified
	var respHeader = this.writer.Header()
	var eTag = ""
	var lastModifiedAt = reader.LastModified()
	if lastModifiedAt > 0 {
		if isWebP {
			eTag = "\"" + strconv.FormatInt(lastModifiedAt, 10) + "_webp" + "\""
		} else {
			eTag = "\"" + strconv.FormatInt(lastModifiedAt, 10) + "\""
		}
		respHeader.Del("Etag")
		respHeader["ETag"] = []string{eTag}
	}

	// 支持 Last-Modified
	// 这里强制设置Last-Modified，如果先前源站设置了Last-Modified，将会被覆盖，避免因为源站的Last-Modified导致源站返回304 Not Modified
	var modifiedTime = ""
	if lastModifiedAt > 0 {
		modifiedTime = time.Unix(utils.GMTUnixTime(lastModifiedAt), 0).Format("Mon, 02 Jan 2006 15:04:05") + " GMT"
		respHeader.Set("Last-Modified", modifiedTime)
	}

	// 支持 If-None-Match
	if len(eTag) > 0 && this.requestHeader("If-None-Match") == eTag {
		// 自定义Header
		this.processResponseHeaders(http.StatusNotModified)
		this.writer.WriteHeader(http.StatusNotModified)
		this.isCached = true
		this.cacheRef = nil
		this.writer.SetOk()
		return true
	}

	// 支持 If-Modified-Since
	if len(modifiedTime) > 0 && this.requestHeader("If-Modified-Since") == modifiedTime {
		// 自定义Header
		this.processResponseHeaders(http.StatusNotModified)
		this.writer.WriteHeader(http.StatusNotModified)
		this.isCached = true
		this.cacheRef = nil
		this.writer.SetOk()
		return true
	}

	this.processResponseHeaders(reader.Status())
	this.addExpiresHeader(reader.ExpiresAt())

	// 输出Body
	if this.RawReq.Method == http.MethodHead {
		this.writer.WriteHeader(reader.Status())
	} else {
		ifRangeHeaders, ok := this.RawReq.Header["If-Range"]
		supportRange := true
		if ok {
			supportRange = false
			for _, v := range ifRangeHeaders {
				if v == this.writer.Header().Get("ETag") || v == this.writer.Header().Get("Last-Modified") {
					supportRange = true
				}
			}
		}

		// 支持Range
		rangeSet := [][]int64{}
		if supportRange {
			fileSize := reader.BodySize()
			contentRange := this.RawReq.Header.Get("Range")
			if len(contentRange) > 0 {
				if fileSize == 0 {
					this.processResponseHeaders(http.StatusRequestedRangeNotSatisfiable)
					this.writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
					return true
				}

				set, ok := httpRequestParseContentRange(contentRange)
				if !ok {
					this.processResponseHeaders(http.StatusRequestedRangeNotSatisfiable)
					this.writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
					return true
				}
				if len(set) > 0 {
					rangeSet = set
					for _, arr := range rangeSet {
						if arr[0] == -1 {
							arr[0] = fileSize + arr[1]
							arr[1] = fileSize - 1

							if arr[0] < 0 {
								this.processResponseHeaders(http.StatusRequestedRangeNotSatisfiable)
								this.writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
								return true
							}
						}
						if arr[1] < 0 {
							arr[1] = fileSize - 1
						}
						if arr[1] >= fileSize {
							arr[1] = fileSize - 1
						}
						if arr[0] > arr[1] {
							this.processResponseHeaders(http.StatusRequestedRangeNotSatisfiable)
							this.writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
							return true
						}
					}
				}
			}
		}

		if len(rangeSet) == 1 {
			respHeader.Set("Content-Range", "bytes "+strconv.FormatInt(rangeSet[0][0], 10)+"-"+strconv.FormatInt(rangeSet[0][1], 10)+"/"+strconv.FormatInt(reader.BodySize(), 10))
			respHeader.Set("Content-Length", strconv.FormatInt(rangeSet[0][1]-rangeSet[0][0]+1, 10))
			this.writer.WriteHeader(http.StatusPartialContent)

			err = reader.ReadBodyRange(buf, rangeSet[0][0], rangeSet[0][1], func(n int) (goNext bool, err error) {
				_, err = this.writer.Write(buf[:n])
				if err != nil {
					return false, errWritingToClient
				}
				return true, nil
			})
			if err != nil {
				this.varMapping["cache.status"] = "MISS"

				if err == caches.ErrInvalidRange {
					this.processResponseHeaders(http.StatusRequestedRangeNotSatisfiable)
					this.writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
					return true
				}
				if !this.canIgnore(err) {
					remotelogs.Warn("HTTP_REQUEST_CACHE", this.URL()+": read from cache failed: "+err.Error())
				}
				return
			}
		} else if len(rangeSet) > 1 {
			boundary := httpRequestGenBoundary()
			respHeader.Set("Content-Type", "multipart/byteranges; boundary="+boundary)
			respHeader.Del("Content-Length")
			contentType := respHeader.Get("Content-Type")

			this.writer.WriteHeader(http.StatusPartialContent)

			for index, set := range rangeSet {
				if index == 0 {
					_, err = this.writer.WriteString("--" + boundary + "\r\n")
				} else {
					_, err = this.writer.WriteString("\r\n--" + boundary + "\r\n")
				}
				if err != nil {
					// 不提示写入客户端错误
					return true
				}

				_, err = this.writer.WriteString("Content-Range: " + "bytes " + strconv.FormatInt(set[0], 10) + "-" + strconv.FormatInt(set[1], 10) + "/" + strconv.FormatInt(reader.BodySize(), 10) + "\r\n")
				if err != nil {
					// 不提示写入客户端错误
					return true
				}

				if len(contentType) > 0 {
					_, err = this.writer.WriteString("Content-Type: " + contentType + "\r\n\r\n")
					if err != nil {
						// 不提示写入客户端错误
						return true
					}
				}

				err := reader.ReadBodyRange(buf, set[0], set[1], func(n int) (goNext bool, err error) {
					_, err = this.writer.Write(buf[:n])
					if err != nil {
						return false, errWritingToClient
					}
					return true, nil
				})
				if err != nil {
					if !this.canIgnore(err) {
						remotelogs.Warn("HTTP_REQUEST_CACHE", this.URL()+": read from cache failed: "+err.Error())
					}
					return true
				}
			}

			_, err = this.writer.WriteString("\r\n--" + boundary + "--\r\n")
			if err != nil {
				this.varMapping["cache.status"] = "MISS"

				// 不提示写入客户端错误
				return true
			}
		} else { // 没有Range
			var resp = &http.Response{Body: reader}
			this.writer.Prepare(resp, reader.BodySize(), reader.Status(), false)
			this.writer.WriteHeader(reader.Status())

			_, err = io.CopyBuffer(this.writer, resp.Body, buf)
			if err == io.EOF {
				err = nil
			}
			if err != nil {
				this.varMapping["cache.status"] = "MISS"

				if !this.canIgnore(err) {
					remotelogs.Warn("HTTP_REQUEST_CACHE", this.URL()+": read from cache failed: read body failed: "+err.Error())
				}
				return
			}
		}
	}

	this.isCached = true
	this.cacheRef = nil

	this.writer.SetOk()

	return true
}

// 设置Expires Header
func (this *HTTPRequest) addExpiresHeader(expiresAt int64) {
	if this.cacheRef.ExpiresTime != nil && this.cacheRef.ExpiresTime.IsPrior && this.cacheRef.ExpiresTime.IsOn {
		if this.cacheRef.ExpiresTime.Overwrite || len(this.writer.Header().Get("Expires")) == 0 {
			if this.cacheRef.ExpiresTime.AutoCalculate {
				this.writer.Header().Set("Expires", time.Unix(utils.GMTUnixTime(expiresAt), 0).Format("Mon, 2 Jan 2006 15:04:05")+" GMT")
			} else if this.cacheRef.ExpiresTime.Duration != nil {
				var duration = this.cacheRef.ExpiresTime.Duration.Duration()
				if duration > 0 {
					this.writer.Header().Set("Expires", utils.GMTTime(time.Now().Add(duration)).Format("Mon, 2 Jan 2006 15:04:05")+" GMT")
				}
			}
		}
	}
}
