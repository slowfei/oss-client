package alibaba

import (
	"context"
	"errors"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"

	"github.com/slowfei/oss-client/pkg/uos"
	"github.com/slowfei/oss-client/pkg/uos/s3common"
)

// mapError 将 alibabacloud-oss-go-sdk-v2 错误翻译为 *uos.Error，
// 遵循 architecture_plan §7.1（14 个冻结的 Code 值）。处理以下情况：
//
//   - 透传已在上游映射的 *uos.Error（capability gating、参数校验等）。
//   - oss.OperationError（v2 SDK 的操作包装器）→ 解包内部错误后继续分类。
//   - oss.ServiceError（v2 SDK 的 wire 错误形态）→ 携带 StatusCode、
//     Code、Message、RequestID、EC。
//   - oss.CanceledError（v2 新增）→ 映射为 context.Canceled
//     或 context.DeadlineExceeded。
//   - 通用回溯到 pkg/uos/s3common 辅助函数（MapCodeString 用于 wire code 字符串，
//     MapHTTPStatus 用于 HTTP 状态回退，MapContextErr 用于 context 取消），
//     然后以 ErrInternal 作为最终的兜底。
//
// architecture_plan §7.1 禁止返回 14 个冻结 Code 之外的任何值；
// 此函数是 alibaba driver 执行该规则的唯一汇聚点。
func mapError(provider uos.Provider, op, bucket, key string, err error) error {
	if err == nil {
		return nil
	}

	// 透传已在上游产生的 *uos.Error。
	var alreadyMapped *uos.Error
	if errors.As(err, &alreadyMapped) {
		// v0.1.1 patch：如果内部 *uos.Error 缺少上下文信息，
		// 用调用方的上下文补充。透传时缺少 Operation/Bucket/Key
		// 发生在内部层（capability gating、参数校验）只构建了 Code+Message 的情况。
		augmented := *alreadyMapped
		if augmented.Provider == "" {
			augmented.Provider = provider
		}
		if augmented.Operation == "" {
			augmented.Operation = op
		}
		if augmented.Bucket == "" {
			augmented.Bucket = bucket
		}
		if augmented.Key == "" {
			augmented.Key = key
		}
		return &augmented
	}

	// v2 SDK 将所有操作错误包装为 oss.OperationError；
	// 需要先解包才能访问内部的 ServiceError / CanceledError 等具体错误。
	inner := err
	var opErr *oss.OperationError
	if errors.As(err, &opErr) {
		inner = opErr.Unwrap()
	}

	out := &uos.Error{
		Provider:  provider,
		Operation: op,
		Bucket:    bucket,
		Key:       key,
		Code:      uos.ErrInternal,
		Message:   err.Error(),
		Cause:     err,
	}

	// oss.ServiceError 是 OSS 数据面错误的主要形态（v2 中为指针类型）。
	// SDK 从 wire 响应填充 StatusCode + Code + Message + RequestID + EC。
	var svcErr *oss.ServiceError
	if errors.As(inner, &svcErr) {
		out.HTTPStatus = svcErr.StatusCode
		out.RequestID = svcErr.RequestID
		// v2 ServiceError 不再有 HostID；将 EC 存入 SecondaryID 供诊断使用。
		if svcErr.EC != "" {
			out.SecondaryID = svcErr.EC
		}
		if svcErr.Message != "" {
			out.Message = svcErr.Message
		}
		out.Code = mapServiceCode(svcErr.Code, svcErr.StatusCode)
		out.Retryable = isRetryable(out.Code, svcErr.StatusCode)
		return out
	}

	// v2 新增 CanceledError（context 取消时产生）。
	// 映射到 s3common.MapContextErr 处理的现有 Code。
	var cancelErr *oss.CanceledError
	if errors.As(inner, &cancelErr) {
		unwrapped := cancelErr.Unwrap()
		if code, ok := s3common.MapContextErr(unwrapped); ok {
			out.Code = code
			out.Retryable = errors.Is(unwrapped, context.DeadlineExceeded)
			return out
		}
		// CanceledError 无更具体信息时，回退为 ErrInternal + 不可重试。
		out.Code = uos.ErrInternal
		out.Retryable = false
		return out
	}

	// Context 取消 / 截止时间超时（最轻量的残余检查；区分 Canceled ——
	// 调用方意图，不可重试 —— 和 DeadlineExceeded —— 暂态，可重试）。
	if code, ok := s3common.MapContextErr(inner); ok {
		out.Code = code
		out.Retryable = errors.Is(inner, context.DeadlineExceeded)
		return out
	}

	// 未映射：保持 ErrInternal；通过 Cause 保留原始错误以便
	// errors.Unwrap / errors.As 调用方仍能看到它。
	return out
}

// mapServiceCode 选择最适合 OSS ServiceError 的冻结 pkg/uos.Code。
// 决策树首先查询共享的 s3common code 表；尚未在共享表中的 OSS 特有 code
// 回退到 HTTP 状态码。未知 code 按文档兜底为 ErrInternal。
//
// 将在 contract testing 中出现但尚未在 s3common.MapCodeString 中的 OSS code
// 列出来是 M3 alibaba 验证目标的一部分 —— 它们在执行时报告出来，
// 以便在 tencent / huawei / volcengine driver 交付之前由 lead
// 在后续提交中扩展 s3common。
func mapServiceCode(code string, status int) uos.Code {
	if mapped, ok := s3common.MapCodeString(code); ok {
		return mapped
	}
	if mapped, ok := s3common.MapHTTPStatus(status); ok {
		return mapped
	}
	return uos.ErrInternal
}

// isRetryable 提示重试是否合理。driver 本身不拥有内部重试器
//（v2 SDK 提供了内置重试器，但 factory.go 中已通过 retry.NopRetryer{} 禁用）；
// 此字段存在是为了让调用方 + pkg/uos.RetryPolicy 能够决策。
// 将每个 Code 的决策委托给 s3common.IsRetryable；无 vendor code 的 HTTP-5xx
// 救援路径保留为内联，因为它依赖 wire 状态（而不仅仅是已解析的 Code）。
func isRetryable(code uos.Code, status int) bool {
	if s3common.IsRetryable(code) {
		return true
	}
	// 某些没有 vendor Code 的 5xx 仍值得重试。
	if status >= 500 && status < 600 {
		return true
	}
	return false
}
