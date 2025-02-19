package checkpoints

import (
	"github.com/TeaOSLab/EdgeNode/internal/waf/requests"
	"github.com/iwind/TeaGo/maps"
	"strings"
)

type RequestHeaderCheckpoint struct {
	Checkpoint
}

func (this *RequestHeaderCheckpoint) RequestValue(req requests.Request, param string, options maps.Map, ruleId int64) (value interface{}, hasRequestBody bool, sysErr error, userErr error) {
	v, found := req.WAFRaw().Header[param]
	if !found {
		value = ""
		return
	}
	value = strings.Join(v, ";")
	return
}

func (this *RequestHeaderCheckpoint) ResponseValue(req requests.Request, resp *requests.Response, param string, options maps.Map, ruleId int64) (value interface{}, hasRequestBody bool, sysErr error, userErr error) {
	if this.IsRequest() {
		return this.RequestValue(req, param, options, ruleId)
	}
	return
}
