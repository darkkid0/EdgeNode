package checkpoints

import (
	"github.com/TeaOSLab/EdgeNode/internal/waf/requests"
	"github.com/iwind/TeaGo/maps"
)

type RequestRemoteAddrCheckpoint struct {
	Checkpoint
}

func (this *RequestRemoteAddrCheckpoint) RequestValue(req requests.Request, param string, options maps.Map, ruleId int64) (value interface{}, hasRequestBody bool, sysErr error, userErr error) {
	value = req.WAFRemoteIP()
	return
}

func (this *RequestRemoteAddrCheckpoint) ResponseValue(req requests.Request, resp *requests.Response, param string, options maps.Map, ruleId int64) (value interface{}, hasRequestBody bool, sysErr error, userErr error) {
	if this.IsRequest() {
		return this.RequestValue(req, param, options, ruleId)
	}
	return
}
