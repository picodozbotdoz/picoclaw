# PicoClaw DeepSeek V4 优化方案

## 文档元数据

| 字段 | 值 |
|------|-----|
| 状态 | 草案 |
| 分支 | `wip/deepseekv4_optimized` |
| 作者 | AI 辅助 |
| 创建日期 | 2026-05-01 |
| 最后更新 | 2026-05-01 |

---

## 1. 概述

DeepSeek V4 引入了 **1,048,576 token 上下文窗口**（1M）、**自动前缀缓存**（缓存命中价格仅为未命中的 1/50）、**384K 最大输出 token** 以及 **三种推理模式**（非思考、高思考、最大思考）。这些能力从根本上改变了 PicoClaw 管理上下文、构建提示、分配 token 预算以及代理循环生命周期的方式。

当前 PicoClaw 架构针对 32K-128K 上下文窗口的模型设计，使用激进的压缩策略、启发式 token 估算，并通过 OpenAI 兼容抽象统一对待所有提供商。DeepSeek V4 的规模需要针对性的优化策略，在保留现有抽象的同时充分利用 V4 的独特特性：超大上下文、自动缓存和推理模式控制。

本方案提出 **8 个工作流**，分为 **4 个阶段**，预计 **6-8 周** 工作量。每个工作流可独立合并，不会破坏现有提供商集成。

---

## 2. DeepSeek V4 技术参考

### 2.1 模型变体

| 模型 | 上下文窗口 | 最大输出 | 活跃参数 | 总参数 |
|------|-----------|---------|---------|--------|
| `deepseek-v4-flash` | 1,048,576 | 384,000 | 13B | 284B |
| `deepseek-v4-pro` | 1,048,576 | 384,000 | 49B | 1.6T |

### 2.2 定价（每百万 token）

| Token 类型 | V4-Flash | V4-Pro |
|-----------|----------|--------|
| 输入（缓存命中） | $0.0028 | $0.003625 |
| 输入（缓存未命中） | $0.14 | $0.435 |
| 输出 | $0.28 | $0.87 |

Flash 上缓存命中价格仅为缓存未命中的 **1/50**。这使得前缀缓存成为影响最大的优化项。

### 2.3 推理模式

| 模式 | API 参数 | 使用场景 |
|------|---------|---------|
| 非思考 | `thinking.type: "disabled"` | 快速工具调用、简单对话、摘要 |
| 高思考（默认） | `thinking.type: "enabled", reasoning_effort: "high"` | 通用推理、多步任务 |
| 最大思考 | `thinking.type: "enabled", reasoning_effort: "max"` | 复杂代理推理、代码生成、深度分析 |

最大思考模式需要 >= 384K 上下文窗口分配。响应包含 `reasoning_content` 和 `content`。

### 2.4 自动前缀缓存

- **机制**："磁盘上下文缓存" —— 自动启用，无需选择加入或特殊请求头
- **范围**：消息数组中重叠的**前缀**部分将被匹配
- **隔离**：KVCache 在 `user_id` 参数级别隔离
- **成本影响**：缓存命中 = Flash 上缓存未命中价格的 1/50
- **关键规则**：消息必须保持相同的顺序和内容在前缀部分才能获得缓存命中

### 2.5 API 兼容性

- **协议**：OpenAI ChatCompletions 兼容
- **基础 URL**：`https://api.deepseek.com`
- **端点**：`POST /chat/completions`
- **`max_tokens` 字段**：使用 `max_tokens`（不是 `max_completion_tokens`）
- **工具调用**：OpenAI 风格函数调用，最多 128 个函数，支持并行工具调用
- **流式输出**：SSE，支持 `stream_options.include_usage: true` 获取 token 用量统计
- **已弃用**：`frequency_penalty` 和 `presence_penalty` 无效

---

## 3. 现有架构差距分析

### 3.1 上下文窗口默认值过于保守

**现状**：`ContextWindow = MaxTokens * 4 = 32768 * 4 = 131,072 token`

**影响**：DeepSeek V4 提供 1M token —— 默认启发式仅分配约 13% 的可用上下文。这导致过早压缩、不必要的摘要，以及丢失本可完整保留的对话历史。

**涉及文件**：`pkg/config/defaults.go`、`pkg/agent/instance.go`

### 3.2 缺少 DeepSeek 专用分词器

**现状**：`EstimateMessageTokens()` 使用 `chars * 2 / 5` 启发式（约 2.5 字符/token）

**影响**：该启发式针对 GPT 类分词器的拉丁文本校准。对于 DeepSeek 的分词器（使用不同的 BPE 合并），估算误差可达 20-40%。过高估算触发不必要压缩；过低估算则有上下文溢出风险。

**涉及文件**：`pkg/agent/context.go`、`pkg/agent/context_usage.go`

### 3.3 未利用 DeepSeek 的提示缓存

**现状**：`supportsPromptCacheKey()` 仅对 OpenAI/Azure 返回 true。DeepSeek 的自动前缀缓存未在应用层面被利用。

**影响**：每次 LLM 调用都支付完整缓存未命中价格，因为消息排序和系统提示构建可能破坏前缀不变性。即使 DeepSeek 自动缓存，应用层消息重排也会使缓存失效。

**涉及文件**：`pkg/agent/prompt.go`、`pkg/agent/prompt_turn.go`、`pkg/providers/openai_compat/provider.go`

### 3.4 缺少推理模式控制

**现状**：PicoClaw 在配置中支持 `ThinkingLevel`，但仅路由到 Anthropic 风格的 `thinking` 块。DeepSeek V4 使用 `thinking.type` + `reasoning_effort`，这是不同的 API 形态。

**影响**：代理无法在同一会话中的非思考（快速工具调用）和最大思考（复杂推理）之间切换，尽管这将是多步代理工作流的最优策略。

**涉及文件**：`pkg/providers/openai_compat/provider.go`、`pkg/config/config_struct.go`、`pkg/agent/pipeline_llm.go`

### 3.5 多轮对话中推理内容未保留

**现状**：`filterDeepSeekReasoningMessages()` 从非工具交互轮次中剥离 `reasoning_content`。

**影响**：DeepSeek V4 API 文档明确指出："在拼接后续多轮消息时，包含 reasoning_content 字段以维护模型推理上下文。" 剥离它导致模型丢失推理链，降低多轮对话的响应质量。

**涉及文件**：`pkg/providers/openai_compat/provider.go`、`pkg/bus/types.go`、`pkg/memory/jsonl.go`

### 3.6 缺少自适应压缩策略

**现状**：压缩在 `compressAt = contextWindow - maxTokens` 时触发。对于 1M 上下文窗口和 32K max_tokens，这意味着输入约 970K token 时才触发压缩 —— 但 `SummarizeMessageThreshold`（20 条消息）和 `SummarizeTokenPercent`（75%）会更早触发。

**影响**：在 1M 上下文下，20 条消息或 75% 上下文处的早期摘要是浪费的。一个 50 轮对话可能轻松容纳在 200K token 内，但当前阈值会在仅 20 条消息后就压缩。

**涉及文件**：`pkg/config/defaults.go`、`pkg/agent/pipeline_setup.go`、`pkg/agent/pipeline_finalize.go`

### 3.7 代理循环中未使用流式输出

**现状**：代理循环调用 `Chat()`（非流式）。`ChatStream()` 在提供商接口中存在但未在流水线中使用。

**影响**：对于 DeepSeek V4 最多 384K 输出 token，非流式调用意味着用户可能需要等待数分钟才能看到任何响应。流式输出还提供 `include_usage` token 计数，可替代启发式估算。

**涉及文件**：`pkg/agent/pipeline_llm.go`、`pkg/providers/openai_compat/provider.go`

### 3.8 缺少上下文窗口分区策略

**现状**：上下文窗口被视为扁平预算，历史消息消耗系统提示 + 工具定义 + max_tokens 预留后的剩余空间。

**影响**：对于 1M 窗口没有刻意的分配策略。处理大型文档或代码库时，没有机制为"检索上下文"与"对话历史"与"工作记忆"预留空间。

**涉及文件**：`pkg/agent/context.go`、`pkg/agent/context_usage.go`

---

## 4. 优化方案

### 阶段 1：基础（第 1-2 周）

这些变更建立 DeepSeek V4 优化的基础设施，不改变其他提供商的现有行为。

---

#### 工作流 1.1：DeepSeek V4 提供商配置

**目标**：创建专用的 DeepSeek V4 提供商配置，正确处理 V4 特定的 API 参数。

**变更**：

1. **在 `pkg/providers/openai_compat/provider.go` 中添加 DeepSeek V4 模型自动检测**：
   - 检测 `deepseek-v4-flash` 和 `deepseek-v4-pro` 模型名称
   - 检测到这些模型时自动设置 `ContextWindow = 1048576`
   - 对 V4 模型使用 `max_tokens` 字段（不是 `max_completion_tokens`）
   - 不发送 `prompt_cache_key`（V4 不支持；会导致 422 错误）

2. **在请求构建器中添加推理模式参数**：
   - 将 PicoClaw 的 `ThinkingLevel` 映射到 DeepSeek V4 的 `thinking` + `reasoning_effort`：
     - `ThinkingLevelNone` → `thinking.type: "disabled"`
     - `ThinkingLevelMedium` → `thinking.type: "enabled", reasoning_effort: "high"`
     - `ThinkingLevelHigh` → `thinking.type: "enabled", reasoning_effort: "max"`
   - 为 DeepSeek V4 模型在请求 JSON 体中添加 `thinking` 和 `reasoning_effort`

3. **在多轮对话中保留 `reasoning_content`**：
   - 对 V4 模型移除或有条件绕过 `filterDeepSeekReasoningMessages()`
   - 将 `reasoning_content` 存储在 `providers.Message.ReasoningContent` 字段中（已存在）
   - 确保在发回 API 的助手消息中包含 `reasoning_content`

4. **添加 `user_id` 参数支持**：
   - 在请求体中传递 `user_id` 以实现多租户场景下的 KV 缓存隔离
   - 从会话密钥或频道+聊天ID 组合派生

**验收标准**：
- DeepSeek V4 模型被正确识别和配置
- 推理模式参数在 API 请求中发送
- `reasoning_content` 在多轮对话中保留
- 非 DeepSeek 模型的现有提供商行为不变

---

#### 工作流 1.2：自适应上下文窗口配置

**目标**：使上下文窗口默认值变为模型感知，而非使用固定乘数。

**变更**：

1. **在 `pkg/config/config_struct.go` 的 `ModelConfig` 中添加 `ModelContextWindow` 字段**：
   - 可选字段，覆盖 `ContextWindow = MaxTokens * 4` 启发式
   - 设置时，优先于启发式和 `AgentDefaults.ContextWindow`
   - 未设置时，回退到当前行为（向后兼容）

2. **在 `pkg/config/defaults.go` 中添加模型特定默认注册表**：
   ```
   deepseek-v4-flash  → context_window: 1048576, max_tokens: 16384
   deepseek-v4-pro    → context_window: 1048576, max_tokens: 16384
   ```
   - 该注册表将模型名称前缀映射到推荐默认值
   - 在模型首次配置时应用，在用户覆盖之前

3. **更新 `pkg/agent/instance.go` 中的 `AgentInstance` 初始化**：
   - 解析上下文窗口时，首先检查 `ModelConfig.ModelContextWindow`
   - 如果支持 > 100K 的模型 `ContextWindow < 100000`，记录警告（可能是配置错误）

4. **在 `ModelConfig` 中添加 `MaxOutputTokens` 字段**：
   - DeepSeek V4 支持最多 384K 输出 token，但默认值应保守（16K）
   - 这与控制每次调用输出生成限制的 `MaxTokens` 分开
   - 显式设置防止在 $0.28/M token 时意外请求 384K 输出

**验收标准**：
- DeepSeek V4 模型自动获得 1M 上下文窗口
- 现有模型保留当前行为
- 用户可以在配置中按模型覆盖上下文窗口
- 可能的配置错误会记录警告

---

### 阶段 2：缓存与压缩（第 3-4 周）

这些变更优化了 DeepSeek V4 缓存和大上下文的提示构建与压缩流水线。

---

#### 工作流 2.1：缓存感知的提示构建

**目标**：构建消息数组以最大化同一会话内顺序 LLM 调用的 DeepSeek V4 前缀缓存命中率。

**变更**：

1. **在 `pkg/agent/prompt.go` 中稳定系统提示排序**：
   - 当前 `PromptRegistry` 按层优先级排序，然后按插槽顺序。这已经是确定性的。
   - 验证缓存部分中没有嵌入动态内容（时间戳、运行时信息）
   - 将所有动态内容（时间、运行时、频道/聊天ID、发送者信息）移到静态系统提示内容之后
   - 结构：`[静态系统提示 | 动态系统提示 | 摘要 | 历史消息 | 用户消息]`
   - 静态系统提示（身份、工作区、技能目录、工具发现）在同一会话的所有调用中应完全相同

2. **在 `BuildMessagesFromPrompt()` 中实现 `CacheBoundary`**：
   - 在消息构建中添加 `CacheBoundary` 标记，指示"稳定前缀"结束的位置
   - 对于 DeepSeek V4，此边界告诉提供商适配器此点之前的所有内容都可缓存
   - 对于 Anthropic，这映射到最后一个稳定内容块的 `CacheControl: ephemeral`
   - 对于 OpenAI，这映射到 `prompt_cache_key`（已支持）

3. **冻结工具定义排序**：
   - `ToolRegistry` 已按确定性方式排序工具名称（字母顺序）
   - 确保此排序在请求体中保留，不仅是提示文本中
   - 工具定义应包含在请求的稳定前缀部分

4. **添加会话级缓存统计跟踪**：
   - 使用 API 响应中的 `usage` 数据跟踪每会话的缓存命中/未命中率
   - 通过 `OutboundMessage` 中的 `ContextUsage` 暴露以供监控
   - 记录缓存命中率帮助用户优化配置

**验收标准**：
- 系统提示前缀在同一会话的顺序调用中完全相同
- 动态内容始终附加在静态内容之后
- 缓存命中率可测量和记录
- 不改变提示内容，仅改变排序和结构

---

#### 工作流 2.2：大上下文自适应压缩策略

**目标**：用上下文感知策略替换固定阈值压缩，为 1M token 模型适当延迟压缩。

**变更**：

1. **在 `pkg/config/config_struct.go` 中添加 `CompressionStrategy` 配置**：
   - `eager`（当前行为）：基于消息计数和 token 百分比提前压缩
   - `adaptive`（新增）：按上下文窗口大小比例缩放阈值
   - `conservative`（新增）：仅在主动预算检查时压缩，从不基于消息计数

2. **在 `pkg/agent/pipeline_finalize.go` 中实现自适应阈值计算**：
   ```
   adaptive_threshold = max(
       SummarizeMessageThreshold,
       context_window / average_turn_tokens * target_fill_percent
   )
   ```
   - 128K 上下文，每轮约 2K token：阈值 ≈ 48 条消息（75% 填充）
   - 1M 上下文，每轮约 2K token：阈值 ≈ 375 条消息（75% 填充）
   - 这防止了大上下文模型上的过早压缩，同时在小型上下文模型上仍会触发

3. **为大上下文更新 `SummarizeTokenPercent`**：
   - 当前：75% 上下文窗口触发摘要
   - 新增：上下文窗口 > 512K 时，提高至 85%（压缩前更多空间）
   - 上下文窗口 > 128K 但 <= 512K 时，提高至 80%
   - 上下文窗口 <= 128K 时，保持 75%（当前行为）

4. **为 DeepSeek V4 添加 `full_context_mode` 选项**：
   - 启用时，完全禁用摘要，保留所有对话历史
   - 仅在上下文溢出时触发紧急压缩
   - 适用于预期使用完整 1M 上下文的会话（如代码库分析）

5. **在压缩中保留 `reasoning_content`**：
   - 压缩包含 `reasoning_content` 的轮次时，在摘要中包含推理的压缩版本
   - 确保模型即使在较旧轮次被摘要后仍保留推理链

**验收标准**：
- 压缩阈值随上下文窗口大小缩放
- 1M 上下文模型上无过早压缩
- 用户可以为 DeepSeek V4 选择完整上下文模式
- <= 128K 上下文模型的现有压缩行为不变

---

### 阶段 3：流水线优化（第 5-6 周）

这些变更针对 DeepSeek V4 的特定能力优化代理循环流水线。

---

#### 工作流 3.1：轮内推理模式切换

**目标**：允许代理在单个轮次的迭代循环中在非思考和最大思考模式之间切换，优化成本和延迟。

**变更**：

1. **在 `processOptions` 中添加 `DynamicThinkingLevel`**：
   - 跟踪每轮迭代内当前思考级别
   - 默认：第一次 LLM 调用使用配置的 `ThinkingLevel`
   - 工具执行后：切换到非思考模式进行下一次 LLM 调用（工具结果不需要深度推理）
   - 无工具调用的迭代后：切换回配置的思考级别

2. **在 `CallLLM()` 中实现思考级别路由**：
   - 每次 LLM 调用前，根据以下条件确定适当的思考级别：
     - 这是轮次中的第一次调用吗？→ 使用配置级别
     - 这是工具调用后的迭代吗？→ 使用非思考（快速）
     - 这是转向恢复迭代吗？→ 使用配置级别
     - 这是压缩后重试吗？→ 使用配置级别
   - 将解析的思考级别传递给提供商适配器

3. **在 `TurnState` 中添加思考模式统计**：
   - 跟踪每次迭代使用的思考级别
   - 包含在轮次完成事件中，用于监控和成本分析

**成本影响分析**（V4-Flash）：
- 非思考：无推理 token，推理更快
- 高思考：中等推理 token，标准定价
- 最大思考：大量推理 token，需要 >= 384K 上下文
- 策略：60-80% 的代理迭代使用非思考（工具结果处理），20-40% 使用思考（初始推理、复杂决策）
- 预计节省：每会话输出 token 成本减少 40-60%

**验收标准**：
- 思考级别在迭代间自动切换
- 工具结果处理默认使用非思考模式
- 用户可以在配置中用固定思考级别覆盖
- 思考模式统计被记录

---

#### 工作流 3.2：代理循环流式集成

**目标**：在代理流水线中使用流式 LLM 调用，减少首 token 时间并提供实时 token 用量数据。

**变更**：

1. **在 `AgentDefaults` 中添加 `StreamingMode`**：
   - `auto`（默认）：支持的模型使用流式，不支持的使用非流式
   - `always`：强制所有提供商使用流式
   - `never`：强制非流式（当前行为）

2. **在 `pkg/agent/pipeline_llm.go` 中实现流式 `CallLLM()`**：
   - 流式启用时，用 `provider.ChatStream()` 替换 `provider.Chat()`
   - 增量处理 SSE 数据块：
     - 累积文本内容用于最终助手消息
     - 累积工具调用增量为完整工具调用
     - 跟踪思考模式的 `reasoning_content` 数据块
   - 为支持实时显示的频道发出部分内容事件
   - 使用 `stream_options.include_usage: true` 从 API 获取准确 token 计数

3. **用 API 报告的用量替换启发式 token 估算**：
   - 使用 `include_usage` 流式输出时，最后一个数据块包含 `prompt_tokens`、`completion_tokens` 和 `prompt_cache_hit_tokens`
   - 使用这些值更新 `ContextUsage` 而非启发式估算
   - API 用量数据不可用时回退到启发式估算

4. **处理保活和超时**：
   - DeepSeek V4 在长时间推理期间发送 SSE 保活注释
   - 实现 10 分钟超时（符合 DeepSeek 文档限制）
   - 超时时，视为瞬态错误并使用指数退避重试

**验收标准**：
- DeepSeek V4 端到端流式工作正常
- token 用量从 API 响应准确跟踪
- 长响应的首 token 时间减少
- 不支持流式的提供商有非流式回退
- 流式禁用时现有行为不变

---

#### 工作流 3.3：上下文窗口分区

**目标**：为 1M token 窗口实现刻意的上下文预算分配，启用"完整代码库在上下文中"工作流。

**变更**：

1. **在 `ModelConfig` 中添加 `ContextPartition` 配置**：
   ```go
   type ContextPartition struct {
       SystemPromptPct   float64  // 系统提示占比（默认：2%）
       WorkingMemoryPct  float64  // 工作记忆/暂存区占比（默认：3%）
       RetrievedContextPct float64  // 注入文档/代码占比（默认：60%）
       HistoryPct        float64  // 对话历史占比（默认：30%）
       OutputPct         float64  // 输出预留占比（默认：5%）
   }
   ```
   - 百分比之和必须为 100%
   - 默认值针对 DeepSeek V4 的 1M 上下文校准

2. **在 `BuildMessagesFromPrompt()` 中实现预算强制**：
   - 组装消息前，计算每个分区的 token 预算
   - 系统提示超出预算时，记录警告但允许溢出到检索上下文
   - 历史消息超出预算时，仅触发最旧轮次的目标压缩
   - 检索上下文超出预算时，使用智能分块截断（保留最相关的部分）

3. **在 `ContextManager` 中添加 `InjectContext()` API**：
   - 用于将大型文档或代码库注入"检索上下文"分区的新方法
   - 支持：文件内容、代码片段、网页内容、PDF 提取
   - 自动管理检索上下文预算，新注入到达时淘汰旧注入
   - 标记为可缓存内容（放置在 DeepSeek V4 缓存的稳定前缀中）

4. **实现 `ContextBudget` 遥测**：
   - 每次 LLM 调用后，报告每个分区的利用率
   - 跟踪：每个分区的实际 token 与预算 token
   - 通过出站消息中的 `ContextUsage` 暴露
   - 任何分区持续超出预算时记录警告

**验收标准**：
- 上下文窗口按可配置预算分区
- 大型文档可注入而不溢出历史分区
- 预算利用率被跟踪和报告
- 默认分区大小在 DeepSeek V4 1M 上下文下工作良好

---

### 阶段 4：高级功能（第 7-8 周）

这些变更添加超越基本优化的 DeepSeek V4 特定功能。

---

#### 工作流 4.1：完整上下文代码库加载

**目标**：为 DeepSeek V4 启用将整个代码库或大型文档加载到上下文中，在许多用例中替代 RAG 分块。

**变更**：

1. **在 `pkg/tools/` 中添加 `context_inject` 工具**：
   - 读取文件/目录并将其注入检索上下文分区的新工具
   - 参数：`path`（文件或目录）、`max_tokens`（预算限制）、`pattern`（目录扫描的 glob）
   - 对于目录：读取所有匹配文件，按相关性排序（修改时间、名称匹配）
   - 遵守 `RetrievedContextPct` 预算；截断或跳过会溢出的文件

2. **添加 `context_list` 工具**：
   - 列出当前注入的上下文项及其 token 计数
   - 显示检索上下文分区中的剩余预算

3. **添加 `context_clear` 工具**：
   - 清除注入的上下文以释放预算
   - 可选 `pattern` 参数选择性清除匹配项

4. **实现智能文件优先级排序**：
   - 当注入目录的文件数超过预算允许时：
     - 优先处理最近修改的文件
     - 优先处理与当前任务描述匹配的文件
     - 跳过二进制文件、`node_modules`、`.git`、vendor 目录
     - 为每个文件内容包含文件路径作为标题

5. **缓存注入的上下文**：
   - 注入的上下文放置在消息数组的稳定前缀中
   - 在后续 LLM 调用中，注入的上下文是缓存命中（前缀匹配）
   - 在 V4-Flash 上重新发送 500K 代码的成本仅为 $0.0014/调用（缓存命中价格）

**成本分析**（V4-Flash）：
- 500K token 注入代码缓存未命中：$0.07/调用
- 500K token 注入代码缓存命中：$0.0014/调用
- 典型 10 次调用会话：完整代码库上下文总计 $0.014（无缓存则 $0.70）
- 对于大多数用例，这比 RAG 嵌入 + 检索更便宜

**验收标准**：
- 整个代码库可通过工具调用加载到上下文中
- 预算被强制执行；溢出得到优雅处理
- 注入的上下文对缓存友好（稳定前缀）
- 清除和重新注入正常工作

---

#### 工作流 4.2：成本感知模型路由

**目标**：基于任务复杂性实现 V4-Flash 和 V4-Pro 之间的智能路由，最大化成本效率。

**变更**：

1. **在 `steering.go` 中扩展成本感知路由**：
   - 添加 `CostAwareRouter`，考虑：
     - 任务复杂性（简单工具调用 vs 复杂推理）
     - 当前会话 token 用量（切换模型会损失多少缓存前缀）
     - 思考级别需求（非思考始终使用 Flash；最大思考可使用 Pro）
     - 速率限制状态（Flash 受限时回退到 Pro）

2. **实现缓存保留的模型切换**：
   - 在会话中从 V4-Flash 切换到 V4-Pro 时：
     - 两个模型共享相同的 API 基础和前缀缓存命名空间
     - 系统提示和注入的上下文在新模型上仍是缓存命中
     - 仅模型处理成本差异变化
   - 切换到完全不同的提供商时：
     - 前缀缓存丢失；适用完整缓存未命中价格
     - 路由器应尽可能优先留在同一提供商系列

3. **在会话配置中添加 `CostBudget`**：
   - 可选的每会话成本限制（以美元计）
     - 接近限制时路由器从 Pro 降级到 Flash
   - 基于 API 报告的用量数据跟踪累计支出

4. **实现复杂性评分**：
   - 基于以下因素对每次 LLM 调用评分：可用工具数、对话深度、输入 token 计数、任务类型
   - 低复杂性（< 0.3）：V4-Flash，非思考
   - 中等复杂性（0.3 - 0.7）：V4-Flash，高思考
   - 高复杂性（> 0.7）：V4-Pro，高思考
   - 关键复杂性（用户明确请求）：V4-Pro，最大思考

**验收标准**：
- 模型路由考虑成本和缓存影响
- Flash 优先用于简单任务；Pro 用于复杂任务
- 在 DeepSeek 系列内切换时保留缓存
- 用户可以设置每会话成本预算

---

#### 工作流 4.3：增强的 Token 用量跟踪和报告

**目标**：提供详细的 token 用量细分，包括缓存命中率、推理 token 成本和每分区利用率。

**变更**：

1. **在 `pkg/bus/types.go` 中扩展 `ContextUsage`**：
   ```go
   type ContextUsage struct {
       UsedTokens          int
       TotalTokens         int
       CompressAtTokens    int
       UsedPercent         float64

       // DeepSeek V4 特定
       CacheHitTokens      int     // 前缀缓存提供的 token
       CacheMissTokens     int     // 新计算的 token
       CacheHitRate        float64 // CacheHitTokens / (CacheHitTokens + CacheMissTokens)
       ReasoningTokens     int     // reasoning_content 使用的 token
       OutputTokens        int     // 最终响应中的 token

       // 分区细分
       SystemPromptTokens  int
       HistoryTokens       int
       InjectedContextTokens int
       ToolDefTokens       int
   }
   ```

2. **从 API 响应解析用量**：
   - DeepSeek V4 返回 `usage.prompt_tokens`、`usage.completion_tokens` 和 `usage.prompt_cache_hit_tokens`
   - 将这些映射到扩展的 `ContextUsage` 字段
   - 流式输出时，从最后的 `include_usage` 数据块提取

3. **添加会话级成本跟踪**：
   - 基于 token 用量和定价计算每会话累计成本
   - 存储在会话元数据中
   - 通过 API 暴露供仪表板显示

4. **在 CLI 中添加 `/cost` 命令**：
   - 显示当前会话成本细分
   - 显示缓存命中率和缓存节省
   - 使用多模型时显示每模型成本分配

**验收标准**：
- Token 用量（包括缓存统计）被准确跟踪
- 会话成本被计算和存储
- 成本细分可通过 CLI 命令访问
- 现有 `ContextUsage` 字段保持向后兼容

---

## 5. 实施顺序与依赖关系

```
阶段 1：基础
├── 1.1 DeepSeek V4 提供商配置 ──────────── [无依赖]
└── 1.2 自适应上下文窗口配置 ────────── [无依赖]

阶段 2：缓存与压缩
├── 2.1 缓存感知提示构建 ────────── [依赖 1.1]
└── 2.2 自适应压缩策略 ──────────── [依赖 1.2]

阶段 3：流水线优化
├── 3.1 推理模式切换 ────────────────── [依赖 1.1]
├── 3.2 流式集成 ───────────────────── [依赖 1.1]
└── 3.3 上下文窗口分区 ─────────────── [依赖 1.2, 2.1]

阶段 4：高级功能
├── 4.1 完整上下文代码库加载 ───────────── [依赖 3.3]
├── 4.2 成本感知模型路由 ────────────────── [依赖 3.1, 4.3]
└── 4.3 增强 Token 用量跟踪 ───────────── [依赖 3.2]
```

工作流 1.1 和 1.2 可并行开发。在阶段 2 中，工作流 2.1 和 2.2 在满足阶段 1 依赖后也可并行。

---

## 6. 风险评估

| 风险 | 可能性 | 影响 | 缓解措施 |
|------|--------|------|---------|
| DeepSeek V4 API 在稳定版发布前变更 | 中 | 高 | 锁定文档 API 规范；添加针对预览 API 的集成测试 |
| 前缀缓存行为与文档不符 | 低 | 中 | 添加缓存命中率监控；命中率 < 20% 时回退到非缓存行为 |
| 1M 上下文导致 PicoClaw 服务器内存压力 | 低 | 高 | 保守设置默认 `MaxTokens`（16K）；添加内存监控 |
| Token 估算不准确导致上下文溢出 | 中 | 中 | 使用 API 报告用量（工作流 3.2）作为主要数据源；保留启发式作为回退 |
| 推理内容膨胀会话存储 | 中 | 低 | 添加可选 `reasoning_content` 压缩；限制每轮存储的推理量 |
| 不受控输出生成导致成本超支 | 中 | 高 | 始终显式设置 `max_tokens`；添加每会话成本预算（工作流 4.2） |

---

## 7. 成功指标

| 指标 | 当前基线 | 目标（阶段 4 后） |
|------|---------|-----------------|
| DeepSeek V4 上的最大有效上下文 | ~128K（启发式限制） | 1M（完整窗口） |
| 前缀缓存命中率 | 不适用（未测量） | > 60%（顺序调用） |
| 压缩触发（1M 上下文） | ~20 条消息 | ~375 条消息（自适应） |
| 推理内容保留 | 被剥离 | 完整保留 |
| 思考模式控制 | 每会话固定 | 每迭代动态 |
| Token 估算准确度 | ~60-80%（启发式） | > 95%（API 报告） |
| 首 token 时间 | 等待完整响应 | < 2 秒（流式） |
| 每 10 轮会话成本（V4-Flash） | ~$0.14（无缓存） | ~$0.03（缓存 + 自适应思考） |
