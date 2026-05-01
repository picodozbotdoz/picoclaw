# PicoClaw DeepSeek V4 优化方案

## 文档元数据

| 字段 | 值 |
|------|-----|
| 状态 | 草案（V4 PDF 审查后修订） |
| 分支 | `wip/deepseekv4_optimized` |
| 作者 | AI 辅助 |
| 创建日期 | 2026-05-01 |
| 最后更新 | 2026-05-01（v2 — 新增 DSML、交错思考、聊天前缀补全、严格模式、快速指令、response_format、reasoning_effort max 系统提示） |

---

## 1. 概述

DeepSeek V4 引入了 **1,048,576 token 上下文窗口**（1M）、**自动前缀缓存**（缓存命中价格仅为未命中的 1/50）、**384K 最大输出 token**、**三种推理模式**（非思考、高思考、最大思考）、**DSML（DeepSeek 标记语言）** 用于基于 XML 的工具调用、**交错思考** 在工具调用边界间保留推理、**聊天前缀补全** 用于引导生成、**严格模式** 用于确定性工具输出，以及 **快速指令 token** 用于辅助任务。这些能力从根本上改变了 PicoClaw 管理上下文、构建提示、分配 token 预算以及代理循环生命周期的方式。

当前 PicoClaw 架构针对 32K-128K 上下文窗口的模型设计，使用激进的压缩策略、启发式 token 估算，并通过 OpenAI 兼容抽象统一对待所有提供商。DeepSeek V4 的规模需要针对性的优化策略，在保留现有抽象的同时充分利用 V4 的独特特性：超大上下文、自动缓存、推理模式控制、基于 DSML 的工具调用和交错思考。

本方案提出 **12 个工作流**，分为 **5 个阶段**，预计 **8-10 周** 工作量。每个工作流可独立合并，不会破坏现有提供商集成。

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
- **JSON 输出**：`response_format: { "type": "json_object" }` 强制有效 JSON 输出
- **Anthropic API 兼容**：DeepSeek 还提供 Anthropic 兼容的 API 端点，便于迁移

### 2.6 DSML（DeepSeek 标记语言）

DeepSeek V4 引入了一种新的基于 XML 的工具调用模式 **DSML**，取代了之前的纯 JSON 格式。虽然 DeepSeek API 接受 OpenAI 兼容的 `tools`/`tool_choice` 参数并在响应中返回结构化的 `tool_calls`，但模型内部使用 DSML 编码。

**DSML 工具调用格式**（内部模型表示）：
```xml
<|DSML|tool_calls>
<|DSML|invoke name="$TOOL_NAME">
<|DSML|parameter name="$PARAMETER_NAME" string="true|false">$PARAMETER_VALUE</|DSML|parameter>
...
</|DSML|invoke>
</|DSML|tool_calls>
```

关键规则：
- `string="true"`：参数值为原始字符串
- `string="false"`：参数值为 JSON（数字、布尔值、数组、对象）
- 工具结果在用户消息中用 `<tool_result>` 标签包裹
- 启用思考模式时，推理必须出现在任何 DSML 工具调用之前的 `<think...</think` 中
- DSML 有效缓解转义失败，相比 JSON 格式减少了工具调用错误

**对 PicoClaw 的影响**：API 在 DSML 和 OpenAI 兼容函数调用格式之间进行转换，因此 PicoClaw 可以继续使用 OpenAI 风格的工具定义。但是，当处理原始模型输出（如本地推理、vLLM）时，PicoClaw 必须直接解析 DSML 格式的响应。

### 2.7 交错思考

DeepSeek V4 对 V3.2 的思考管理策略进行了改进，具有两种不同的行为：

1. **工具调用场景**：所有推理内容在整个对话中完全保留，包括跨用户消息边界。与 V3.2 在每个新用户轮次时丢弃思考轨迹不同，V4 在存在工具时保留所有轮次的完整推理历史。这使模型能够在长时间跨度的代理任务中维持连贯的、累积的思维链。

2. **一般对话场景**：当新用户消息到达时，先前轮次的推理内容被丢弃，保持上下文简洁，适用于持续推理轨迹收益有限的场景。

`drop_thinking` 编码参数控制此行为：
- `drop_thinking=True`（非工具对话的默认值）：剥离早期轮次的推理
- `drop_thinking=False`（存在工具时自动启用）：保留所有推理内容

**对 PicoClaw 的影响**：当系统消息中定义了工具时，PicoClaw 不得从任何轮次中剥离 `reasoning_content`。当前的 `filterDeepSeekReasoningMessages()` 函数必须感知对话上下文中是否存在工具。

### 2.8 聊天前缀补全（Beta）

允许预填充助手响应以引导生成。使用助手消息上的 `prefix: true` 参数强制模型从该前缀开始补全。

- **Beta 端点**：`base_url="https://api.deepseek.com/beta"`
- **使用场景**：强制代码输出格式（例如前缀为 `` ```python\n ``）、引导结构化响应、实现约束生成
- **思考模式集成**：最后一条助手消息上的 `reasoning_content` 字段可用作 CoT 前缀输入
- **停止序列**：与 `stop` 参数结合实现精确输出控制

### 2.9 工具调用严格模式（Beta）

当函数定义上设置 `strict: true` 时，API 验证工具调用输出是否完全符合函数的 JSON Schema。这消除了格式错误的工具调用。

- **Beta 端点**：`base_url="https://api.deepseek.com/beta"`
- **要求**：所有属性必须在 `required` 中，每个对象设置 `additionalProperties: false`
- **支持的 Schema 类型**：`object`、`string`、`number`、`integer`、`boolean`、`array`、`enum`、`anyOf`
- **在两种模式下均可使用**：思考模式和非思考模式
- **验证**：服务端 Schema 验证拒绝无效 Schema 并返回错误消息

### 2.10 快速指令 Token

附加到消息上的特殊 token，用于辅助分类和生成任务。这些 token 利用已计算的 KV 缓存，避免冗余预填充并降低 TTFT。

| Token | 用途 | 格式 |
|-------|------|------|
| `<\|action\|>` | 路由：网络搜索 vs 直接回答 | `...<\|User\|>{prompt}<\|Assistant\|><think<\|action\|>` |
| `<\|title\|>` | 生成对话标题 | `...<\|Assistant\|>{response}<\|end_of_sentence\|><\|title\|>` |
| `<\|query\|>` | 生成搜索查询 | `...<\|User\|>{prompt}<\|query\|>` |
| `<\|authority\|>` | 分类来源权威性 | `...<\|User\|>{prompt}<\|authority\|>` |
| `<\|domain\|>` | 识别提示领域 | `...<\|User\|>{prompt}<\|domain\|>` |
| `<\|extracted_url\|><\|read_url\|>` | URL 获取决策 | `...<\|User\|>{prompt}<\|extracted_url\|>{url}<\|read_url\|>` |

### 2.11 推理努力：最大模式系统提示注入

当设置 `reasoning_effort: "max"` 时，API 自动在对话最开头（用户系统提示之前）预置一条特殊的系统提示指令：

```
Reasoning Effort: Absolute maximum with no shortcuts permitted.
You MUST be very thorough in your thinking and comprehensively decompose the
problem to resolve the root cause, rigorously stress-testing your logic against all
potential paths, edge cases, and adversarial scenarios.
Explicitly write out your entire deliberation process, documenting every intermediate
step, considered alternative, and rejected hypothesis to ensure absolutely no
assumption is left unchecked.
```

**对 PicoClaw 的影响**：此前缀由 API 服务端管理。PicoClaw 不应手动注入此文本。但是，PicoClaw 必须在计算上下文预算时考虑此前缀添加的额外 token。

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

### 3.9 缺少 DSML 工具调用解析

**现状**：PicoClaw 假设所有提供商以 OpenAI 兼容的 JSON 格式返回工具调用（`tool_calls` 数组，包含 `function.name` 和 `function.arguments`）。DeepSeek V4 的内部 DSML 格式未被处理。

**影响**：当使用本地推理（vLLM、Ollama）运行 DeepSeek V4 模型时，模型输出可能包含 DSML 格式的工具调用（`<|DSML|tool_calls>...`）而非结构化 JSON。PicoClaw 无法解析这些，导致本地部署的代理循环中断。即使使用云 API，理解 DSML 对于调试原始模型输出和解析可能包含部分 DSML 片段的 `reasoning_content` 也很重要。

**涉及文件**：`pkg/providers/openai_compat/provider.go`、`pkg/providers/openai_compat/dsml_parser.go`（新增）

### 3.10 交错思考未正确处理

**现状**：`filterDeepSeekReasoningMessages()` 根据轮次是否涉及工具交互来剥离 `reasoning_content`，但未考虑 V4 中工具调用场景（保留所有推理）与一般对话场景（丢弃早期推理）的区别。

**影响**：在 V4 中，当存在工具时，所有轮次的所有推理内容（包括跨用户消息边界）都必须保留。当前代码可能仍会不当剥离推理，破坏 V4 的交错思考机制。这降低了模型在多步代理任务中维持连贯推理的能力。

**涉及文件**：`pkg/providers/openai_compat/provider.go`、`pkg/agent/pipeline_llm.go`

### 3.11 缺少聊天前缀补全支持

**现状**：PicoClaw 没有使用 `prefix: true` 参数预填充助手响应的机制。

**影响**：聊天前缀补全可启用引导生成模式（如强制代码输出格式、约束 JSON 结构），减少提示工程复杂度并提高输出可靠性。没有此功能，PicoClaw 仅依靠提示指令进行格式控制，可靠性较低。

**涉及文件**：`pkg/providers/openai_compat/provider.go`、`pkg/agent/pipeline_llm.go`

### 3.12 缺少工具调用严格模式

**现状**：工具函数定义发送时未包含 `strict: true` 参数，意味着模型的工具调用输出未根据 JSON Schema 进行验证。

**影响**：没有严格模式，工具调用偶尔可能产生格式错误的 JSON 参数（缺失字段、错误类型），导致工具执行时的运行时错误。严格模式将完全消除此类错误，这对于自主代理工作流至关重要，因为一个格式错误的工具调用就可能使整个任务脱轨。

**涉及文件**：`pkg/providers/openai_compat/provider.go`、`pkg/tools/registry.go`

### 3.13 缺少 JSON 输出模式支持

**现状**：PicoClaw 不在 API 请求中发送 `response_format: { "type": "json_object" }`。

**影响**：某些 PicoClaw 功能（结构化数据提取、工具参数解析）将受益于保证的 JSON 输出。没有此功能，模型可能在期望 JSON 时生成自由文本，需要额外的解析和错误处理。

**涉及文件**：`pkg/providers/openai_compat/provider.go`、`pkg/agent/pipeline_llm.go`

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
   - **交错思考**：当系统消息中存在工具时，跨所有轮次保留所有 `reasoning_content`（包括跨用户消息边界）。这实现了 V4 的"工具调用场景"行为，模型维持累积的思维链
   - 当不存在工具时，应用 `drop_thinking` 行为：剥离最后一条用户消息之前轮次的推理，仅保留最近助手轮次的推理

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

### 阶段 5：DSML 与高级 V4 功能（第 9-10 周）

这些变更添加在 PDF 文档审查期间发现的 DeepSeek V4 特定功能：DSML 工具调用解析、聊天前缀补全、工具调用严格模式和 JSON 输出模式支持。

---

#### 工作流 5.1：DSML 工具调用解析器

**目标**：为 DeepSeek V4 的 DSML（DeepSeek 标记语言）工具调用格式实现解析器，同时兼容云 API 和本地推理部署。

**变更**：

1. **创建 `pkg/providers/openai_compat/dsml_parser.go`**：
   - 实现 `ParseDSMLToolCalls(content string) ([]ToolCall, error)`，从 DSML 格式文本中提取工具调用：
     ```
     <|DSML|tool_calls>
     <|DSML|invoke name="function_name">
     <|DSML|parameter name="param" string="true">string_value</|DSML|parameter>
     <|DSML|parameter name="count" string="false">5</|DSML|parameter>
     </|DSML|invoke>
     </|DSML|tool_calls>
     ```
   - 将每个 `<|DSML|invoke>` 块解析为结构化的 `ToolCall`，包含 `name` 和 `arguments`（JSON 对象）
   - 对于 `string="true"` 参数：将原始值包装为 JSON 字符串
   - 对于 `string="false"` 参数：直接将值解析为 JSON
   - 处理单个 `<|DSML|tool_calls>` 块中的多个 `<|DSML|invoke>` 块

2. **创建 `pkg/providers/openai_compat/dsml_parser_test.go`**：
   - 测试包含字符串和非字符串参数的单个工具调用
   - 测试单个 DSML 块中的多个工具调用
   - 测试嵌套 JSON 参数（数组、对象）
   - 测试格式错误的 DSML（未闭合标签、缺失属性）
   - 测试 DSML 与常规文本内容混合
   - 测试 `reasoning_content` 中的 DSML（应原样保留）

3. **在 `pkg/providers/openai_compat/provider.go` 中集成 DSML 解析器到响应处理**：
   - 收到响应后，检查 `content` 是否包含 `<|DSML|tool_calls>` 标记
   - 如果找到 DSML 标记且 `tool_calls` 数组为空，解析 DSML 以填充 `tool_calls`
   - 这处理本地推理引擎返回 DSML 而非结构化 JSON 的情况
   - 对于云 API 响应，`tool_calls` 数组已填充；DSML 解析作为回退

4. **添加 DSML 感知的调试日志**：
   - 在响应中检测到 DSML 内容时，以调试级别记录解析的工具调用
   - 这有助于调试本地推理部署

**验收标准**：
- DSML 格式的工具调用被正确解析为 OpenAI 兼容的 `ToolCall` 结构
- 云 API 响应不变（DSML 解析仅作为回退）
- DSML 格式的本地推理响应被正确处理
- 解析器处理边缘情况（格式错误的 XML、混合内容、嵌套参数）

---

#### 工作流 5.2：聊天前缀补全支持

**目标**：使 PicoClaw 能够使用 DeepSeek V4 的聊天前缀补全功能进行引导生成。

**变更**：

1. **在 `pkg/providers/openai_compat/provider.go` 中添加 `PrefixCompletion` 选项到 `ProviderOptions`**：
   - 新选项：`prefix_completion_content string` —— 用作助手前缀的内容
   - 设置后，请求中的最后一条助手消息将带有 `prefix: true`
   - 需要使用 Beta 端点：`base_url="https://api.deepseek.com/beta"`

2. **在请求构建器中实现前缀补全**：
   - 附加带有前缀内容和 `prefix: true` 的助手消息
   - 可选设置 `stop` 序列以控制生成结束位置
   - 示例：通过将前缀设为 `` ```python\n ``、停止序列设为 `` ``` `` 来强制 Python 代码输出

3. **添加 `reasoning_content` 前缀支持**：
   - 最后一条助手消息上的 `reasoning_content` 字段可用作 CoT 前缀
   - 启用 `thinking_mode` 时，允许同时提供 `reasoning_content` 和内容前缀
   - 这实现了"引导推理"，模型从部分编写的推理链继续

4. **添加 Beta 端点检测**：
   - 请求前缀补全时，自动切换到 `https://api.deepseek.com/beta`
   - 记录正在使用 Beta 功能的警告
   - Beta 端点返回错误时优雅回退

**验收标准**：
- 前缀补全可用于引导输出格式
- 需要时自动使用 Beta 端点
- 常规（非前缀）补全不受影响
- 在思考模式和非思考模式下均可工作

---

#### 工作流 5.3：工具调用严格模式

**目标**：为 DeepSeek V4 工具调用启用严格模式以保证 Schema 一致的输出。

**变更**：

1. **在 `pkg/config/config_struct.go` 中添加 `StrictToolCalls` 选项到 `ModelConfig`**：
   - 当为 `true` 时，发送给 DeepSeek V4 的所有工具函数定义将包含 `strict: true`
   - 默认：`false`（向后兼容）

2. **在 `pkg/providers/openai_compat/provider.go` 中实现严格模式请求构建**：
   - 当 DeepSeek V4 模型启用 `StrictToolCalls` 时：
     - 在 `tools` 数组的每个函数定义中添加 `"strict": true`
     - 确保所有对象 Schema 具有 `additionalProperties: false` 且所有属性在 `required` 中
     - 发送前本地验证 Schema；对不合规 Schema 记录警告
   - 使用 Beta 端点：`base_url="https://api.deepseek.com/beta"`

3. **在 `pkg/tools/schema_validator.go`（新增）中添加 Schema 验证辅助工具**：
   - 验证工具参数 Schema 是否符合严格模式要求
   - 检查：所有对象属性在 `required` 中，每个对象设置 `additionalProperties: false`
   - 支持的类型：`object`、`string`、`number`、`integer`、`boolean`、`array`、`enum`、`anyOf`
   - 返回具有可操作消息的验证错误（如"属性 'name' 缺失于 required 数组"）

4. **处理严格模式验证错误**：
   - 如果 API 返回 Schema 验证错误，记录包含具体函数和参数的错误
   - 对该函数回退到非严格模式并重试
   - 这防止单个不合规 Schema 阻塞整个请求

**验收标准**：
- 严格模式保证 Schema 一致的工具调用输出
- 不合规 Schema 在 API 提交前被检测和报告
- 到非严格模式的回退优雅工作
- 严格模式禁用时现有工具定义不变

---

#### 工作流 5.4：JSON 输出模式

**目标**：支持 DeepSeek V4 的 `response_format` 参数以实现保证的 JSON 输出。

**变更**：

1. **在 `pkg/providers/openai_compat/provider.go` 中添加 `ResponseFormat` 选项到 `ProviderOptions`**：
   - 新选项：`response_format string` —— `"text"`（默认）或 `"json_object"`
   - 当为 `"json_object"` 时，在请求体中添加 `response_format: { "type": "json_object" }`

2. **与代理流水线集成**：
   - 某些工具或流水线阶段可请求 JSON 输出模式（如结构化数据提取）
   - JSON 输出模式激活时，确保系统或用户消息包含 JSON 格式指令（API 要求）
   - 添加响应是否为有效 JSON 的验证，然后再处理

3. **在 `ModelConfig` 中添加 `ResponseFormat`**：
   - 可选字段：`response_format string`
   - 在配置级别设置时，应用于该模型的所有请求
   - 可在流水线选项中按请求覆盖

**验收标准**：
- JSON 输出模式强制有效 JSON 响应
- 模式激活时系统提示包含 JSON 指令
- 响应验证优雅捕获非 JSON 输出
- 默认行为（文本模式）不变

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

阶段 5：DSML 与高级 V4 功能
├── 5.1 DSML 工具调用解析器 ───────────── [依赖 1.1]
├── 5.2 聊天前缀补全 ──────────────────── [依赖 1.1]
├── 5.3 工具调用严格模式 ──────────────── [依赖 1.1, 5.1]
└── 5.4 JSON 输出模式 ────────────────── [依赖 1.1]
```

工作流 1.1 和 1.2 可并行开发。在阶段 2 中，工作流 2.1 和 2.2 在满足阶段 1 依赖后也可并行。阶段 5 的工作流 5.1、5.2 和 5.4 可并行开发；5.3 依赖 5.1（严格模式集成测试需要 DSML 解析器）。

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
| DSML 解析在格式错误的模型输出上出错 | 中 | 中 | 全面的测试覆盖；优雅回退到原始文本；记录 DSML 解析失败 |
| Beta API 功能（前缀补全、严格模式）可能变更 | 中 | 中 | 通过配置标志门控；记录为 Beta；提供回退路径 |
| 交错思考显著增加 token 用量 | 高 | 中 | 跟踪推理 token 成本；添加每会话推理 token 预算限制 |

---

## 7. 测试策略

### 单元测试

- DeepSeek V4 模型检测和配置
- 思考级别映射（PicoClaw → DeepSeek API 参数）
- 自适应压缩阈值计算
- 上下文分区预算强制
- 缓存稳定性的消息排序
- DSML 工具调用解析（单个、多个、格式错误、混合内容）
- 严格模式 Schema 验证
- JSON 输出模式请求构建
- 聊天前缀补全请求构建
- 交错思考：工具存在时 vs 不存在时的推理保留

### 集成测试

- 端到端 DeepSeek V4 API 调用（非思考、高思考、最大思考）
- 多轮对话中 `reasoning_content` 保留
- 工具调用的流式输出
- 前缀缓存命中率测量（需要顺序调用）
- 上下文注入和预算管理
- 本地推理的 DSML 格式响应解析
- 严格模式工具调用与 Schema 验证
- 聊天前缀补全与引导输出
- JSON 输出模式与响应验证

### 负载测试

- 持续 1M token 上下文会话（内存、延迟）
- 高并发多租户会话与 `user_id` 隔离
- 真实对话模式下的缓存命中率

### 成本验证测试

- 比较估算成本与实际 API 计费
- 验证缓存命中价格正确应用
- 测试成本预算强制

---

## 8. 配置示例

### 最小 DeepSeek V4 配置

```json
{
  "models": [
    {
      "name": "deepseek-v4-flash",
      "model": "deepseek/deepseek-v4-flash",
      "api_base": "https://api.deepseek.com",
      "api_key": "${DEEPSEEK_API_KEY}",
      "max_tokens": 16384,
      "context_window": 1048576
    }
  ],
  "defaults": {
    "model": "deepseek-v4-flash",
    "max_tokens": 16384,
    "thinking_level": "medium",
    "compression_strategy": "adaptive"
  }
}
```

### 完整 DeepSeek V4 配置与路由

```json
{
  "models": [
    {
      "name": "deepseek-v4-flash",
      "model": "deepseek/deepseek-v4-flash",
      "api_base": "https://api.deepseek.com",
      "api_key": "${DEEPSEEK_API_KEY}",
      "max_tokens": 16384,
      "context_window": 1048576,
      "thinking_level": "medium"
    },
    {
      "name": "deepseek-v4-pro",
      "model": "deepseek/deepseek-v4-pro",
      "api_base": "https://api.deepseek.com",
      "api_key": "${DEEPSEEK_API_KEY}",
      "max_tokens": 32768,
      "context_window": 1048576,
      "thinking_level": "high",
      "strict_tool_calls": true
    }
  ],
  "defaults": {
    "model": "deepseek-v4-flash",
    "max_tokens": 16384,
    "thinking_level": "medium",
    "compression_strategy": "adaptive",
    "streaming_mode": "auto",
    "context_partitions": {
      "system_prompt_pct": 2,
      "working_memory_pct": 3,
      "retrieved_context_pct": 60,
      "history_pct": 30,
      "output_pct": 5
    }
  },
  "router": {
    "light_candidates": ["deepseek-v4-flash"],
    "heavy_candidates": ["deepseek-v4-pro"],
    "cost_budget_usd": 1.0
  }
}
```

### 高级 V4 功能配置

```json
{
  "models": [
    {
      "name": "deepseek-v4-pro-strict",
      "model": "deepseek/deepseek-v4-pro",
      "api_base": "https://api.deepseek.com/beta",
      "api_key": "${DEEPSEEK_API_KEY}",
      "max_tokens": 32768,
      "context_window": 1048576,
      "thinking_level": "high",
      "strict_tool_calls": true,
      "response_format": "json_object"
    }
  ]
}
```

---

## 9. 成功指标

| 指标 | 当前基线 | 目标（阶段 5 后） |
|------|---------|-----------------|
| DeepSeek V4 上的最大有效上下文 | ~128K（启发式限制） | 1M（完整窗口） |
| 前缀缓存命中率 | 不适用（未测量） | > 60%（顺序调用） |
| 压缩触发（1M 上下文） | ~20 条消息 | ~375 条消息（自适应） |
| 推理内容保留 | 被剥离 | 完整保留（含交错思考） |
| 思考模式控制 | 每会话固定 | 每迭代动态 |
| Token 估算准确度 | ~60-80%（启发式） | > 95%（API 报告） |
| 首 token 时间 | 等待完整响应 | < 2 秒（流式） |
| 每 10 轮会话成本（V4-Flash） | ~$0.14（无缓存） | ~$0.03（缓存 + 自适应思考） |
| DSML 工具调用解析 | 不支持 | 完整支持（本地 + 云） |
| 严格模式工具调用 | 不支持 | Schema 验证的工具输出 |
| 聊天前缀补全 | 不支持 | 代码/结构化输出的引导生成 |
| JSON 输出模式 | 不支持 | 保证 JSON 响应 |

---

## 10. 不在范围内

以下内容明确排除在本方案之外：

- **RAG 替代**：完整上下文加载减少但不消除对 RAG 的需求。超大型代码库（> 1M token）仍需检索。
- **DeepSeek V4 微调**：本方案专注于 API 优化，而非模型定制。
- **多模型共识**：并行运行 V4-Flash 和 V4-Pro 处理相同查询不在计划内。
- **批处理**：DeepSeek 不提供批处理 API；实现自定义批处理队列不在范围内。
- **本地部署**：本方案针对 DeepSeek V4 云 API。本地部署优化是独立课题。
- **Web UI 变更**：Web 前端（`web/`）不需要为 V4 优化进行变更。上下文用量显示将使用现有 `ContextUsage` 字段。
- **快速指令 token**：`<|action|>`、`<|title|>`、`<|query|>`、`<|authority|>`、`<|domain|>`、`<|extracted_url|>` 和 `<|read_url|>` token 是 DeepSeek 内部聊天机器人辅助任务的流水线 token。在 2.10 节中记录以供参考，但在 PicoClaw 中实现它们不在范围内，因为 PicoClaw 不运行带有搜索/标题生成功能的聊天机器人 UI。如果将来需要，将作为独立功能处理。
- **`developer` 角色**：`developer` 消息角色仅在 DeepSeek 内部搜索代理流水线中使用。官方 API 不接受此角色的消息。PicoClaw 不应发送 `developer` 角色的消息。
- **`latest_reminder` 角色**：`latest_reminder` 角色注入日期/区域设置上下文。PicoClaw 通过自身的系统提示构建以不同方式处理此问题，因此不需要 V4 特定的角色。
- **Anthropic API 兼容端点**：DeepSeek 提供 Anthropic 兼容的 API，但 PicoClaw 已有原生 Anthropic 提供商实现，使用 DeepSeek Anthropic 包装器没有收益。
- **FIM（中间填充）补全**：这是用于代码补全任务的独立 Beta 功能，而非基于聊天的编码辅助。如果 PicoClaw 未来添加内联代码补全功能，可能会重新考虑。
