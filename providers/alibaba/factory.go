package alibaba

import (
	"context"
	"fmt"
	"strings"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/retry"

	"github.com/slowfei/oss-client/pkg/uos"
	"github.com/slowfei/oss-client/pkg/uos/credential"
)

// providerID 是此 driver 注册的规范 Provider 标识。
// 写死为常量，确保编译时检查能捕获变更。
const providerID uos.Provider = "alibaba"

// DriverConfig 是 Alibaba 专属的选项集。调用方在 uos.Config.DriverConfig
// 上设置此值；Factory.Validate 通过类型断言校验。所有字段均为可选；
// 零值即可得到一个可工作的虚拟主机模式 OSS driver（前提是 uos.Config 上
// 设置了 Region 或 Endpoint）。
//
// v2 SDK 已将 UseCNAME / PathStyle / AuthVersion / DisableSSLVerify
// 等配置项提升为 oss.Config 的一等字段；DriverConfig 保留为扩展预留。
type DriverConfig struct {
	// Extra 是透传给 v2 Client Options 的回调，用于注入高级配置
	//（例如覆盖 Endpoint、注入自定义 HTTPClient 等）。
	// 为空时使用默认行为。
	Extra func(*oss.Options)
}

// Factory 返回 Alibaba OSS driver 的 uos.Factory。
// driver 在 init 时自行注册（调用方也可手动注册）：
//
//	uos.DefaultRegistry().Register(alibaba.Factory())
func Factory() uos.Factory { return factoryImpl{} }

// factoryImpl 是 Alibaba OSS 的具体 uos.Factory 实现。
type factoryImpl struct{}

// init 将此 driver 注册到进程级 Registry。
// 不想产生全局副作用的测试和调用方应构造独立的 Registry
// 并通过 uos.NewRegistry 手动 Register(Factory())。
func init() {
	_ = uos.DefaultRegistry().Register(factoryImpl{})
}

// Provider 返回规范 provider 标识（"alibaba"）。实现 uos.Factory 接口。
func (factoryImpl) Provider() uos.Provider { return providerID }

// Validate 检查 cfg 的结构合法性，不执行任何网络 I/O。
// Region 或 Endpoint 至少有一个必须设置；CredentialProvider 是必选的
//（OSS 对合约套件涉及的访问操作拒绝匿名请求）。
// DriverConfig 如非 nil 则必须为 *DriverConfig。
func (factoryImpl) Validate(cfg uos.Config) error {
	if cfg.Provider != "" && cfg.Provider != providerID {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message: fmt.Sprintf(
				"Config.Provider=%q does not match this Factory (%q)",
				string(cfg.Provider), string(providerID),
			),
		}
	}
	if strings.TrimSpace(cfg.Region) == "" && strings.TrimSpace(cfg.Endpoint) == "" {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   "Config.Region or Config.Endpoint is required for the alibaba driver",
		}
	}
	if cfg.CredentialProvider == nil {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   "Config.CredentialProvider is required for the alibaba driver",
		}
	}
	if cfg.DriverConfig != nil {
		if _, ok := cfg.DriverConfig.(*DriverConfig); !ok {
			return &uos.Error{
				Code:      uos.ErrInvalidArgument,
				Provider:  providerID,
				Operation: "Factory.Validate",
				Message:   fmt.Sprintf("DriverConfig must be *alibaba.DriverConfig, got %T", cfg.DriverConfig),
			}
		}
	}
	return nil
}

// Open 执行凭证探测并构造底层的 *oss.Client，封装为 uos.Client 返回。
// 处理以下配置：
//
//   - cfg.Endpoint 作为 OSS endpoint URL（例如 "https://oss-cn-hangzhou.aliyuncs.com"）；
//     为空时从 cfg.Region 推导为 "https://oss-<region>.aliyuncs.com"。
//   - cfg.CredentialProvider 用于获取 AK/SK + 可选的 STS session token。
//   - DriverConfig.Extra 用于注入 v2 Client Options 回调。
//
// v2 SDK 内置请求级重试器（retry.NewStandard）；本项目采用 pkg/uos.RetryPolicy
// 作为权威重试入口，所以必须显式禁用 v2 重试以避免"双重重试"风险
//（见 docs/provider_roadmap.md cross-cutting risk）。
//
// 签名算法默认设置为 SignatureVersionV1，与 v1 SDK 的 AuthV1 行为保持一致；
// 调用方可通过 DriverConfig.Extra 覆盖为 SignatureVersionV4。
func (f factoryImpl) Open(_ context.Context, cfg uos.Config) (uos.Client, error) {
	if err := f.Validate(cfg); err != nil {
		return nil, err
	}

	cred, err := cfg.CredentialProvider.Resolve(context.Background(), string(providerID))
	if err != nil {
		return nil, &uos.Error{
			Code:      uos.ErrUnauthenticated,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   "credential provider failed",
			Cause:     err,
		}
	}

	akid, secret, token, err := extractHMAC(cred)
	if err != nil {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   err.Error(),
			Cause:     err,
		}
	}

	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = fmt.Sprintf("https://oss-%s.aliyuncs.com", cfg.Region)
	}

	dc, _ := cfg.DriverConfig.(*DriverConfig)
	if dc == nil {
		dc = &DriverConfig{}
	}

	v2cfg := oss.NewConfig().
		WithRegion(cfg.Region).
		WithEndpoint(endpoint).
		WithCredentialsProvider(credentials.NewStaticCredentialsProvider(akid, secret, token)).
		WithRetryer(retry.NopRetryer{}).
		WithSignatureVersion(oss.SignatureVersionV1)

	// 构建 v2 Client。Extra 回调仅在非 nil 时传入，避免将 nil 函数作为
	// variadic 参数传递导致 NewClient 内部 for-range 时 panic。
	var optFns []func(*oss.Options)
	if dc.Extra != nil {
		optFns = append(optFns, dc.Extra)
	}

	client := oss.NewClient(v2cfg, optFns...)

	return &driverImpl{
		cfg:    cfg,
		client: client,
	}, nil
}

// extractHMAC 将 Credential 解包为此 driver 需要的 (access key, secret, token) 三元组。
// *credential.EnvHMACCredential 是参考 HMAC 载荷形状；函数也接受值形式以便调用方使用，
// 对未知载荷形状返回明确错误。
func extractHMAC(c credential.Credential) (akid, secret, token string, err error) {
	if c.Scheme != "" && c.Scheme != credential.AuthHMAC {
		return "", "", "", fmt.Errorf("alibaba driver requires AuthHMAC credentials, got %q", string(c.Scheme))
	}
	switch v := c.Opaque.(type) {
	case *credential.EnvHMACCredential:
		if v == nil || v.AccessKeyID == "" || v.SecretAccessKey == "" {
			return "", "", "", fmt.Errorf("alibaba driver: HMAC credential missing access key or secret")
		}
		return v.AccessKeyID, v.SecretAccessKey, v.SessionToken, nil
	case credential.EnvHMACCredential:
		if v.AccessKeyID == "" || v.SecretAccessKey == "" {
			return "", "", "", fmt.Errorf("alibaba driver: HMAC credential missing access key or secret")
		}
		return v.AccessKeyID, v.SecretAccessKey, v.SessionToken, nil
	default:
		return "", "", "", fmt.Errorf(
			"alibaba driver: unsupported credential opaque type %T (need *credential.EnvHMACCredential)",
			c.Opaque,
		)
	}
}

// 编译时保证。
var _ uos.Factory = factoryImpl{}
