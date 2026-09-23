package token

import "sync"

// endpointVar 让测试能把端点指到桩服务器。
// 用变量而非改写 Endpoint 常量，保持生产路径是 const。
var (
	endpointMu sync.RWMutex
	endpoint   = Endpoint
)

func setEndpoint(u string) {
	endpointMu.Lock()
	defer endpointMu.Unlock()
	endpoint = u
}

func currentEndpoint() string {
	endpointMu.RLock()
	defer endpointMu.RUnlock()
	return endpoint
}
