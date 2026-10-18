# z-coding-plan

CLIProxyAPI 插件。展示智谱 GLM Coding Plan 额度，并在智谱返回 1308 且带重置时间时，停用那把实际发出请求的凭证，直到该时间。

只支持中国区 BigModel。额度固定请求 https://bigmodel.cn/api/monitor/usage/quota/limit。不支持全球区 api.z.ai，也没有额度地址配置项。中国区 Key 不能拿到全球区使用，反过来也不行。

这里有两份不同的凭证，不要混用：

- 额度认证文件是插件解析的 JSON。它只让管理页的额度页能显示剩余额度，本身不是路由 Key。
- 路由凭证是 CLIProxyAPI 用来请求智谱的上游 API Key。1308 切换跟着实际发出那次请求的凭证 id 走。

## 配置

在 CLIProxyAPI 的配置目录操作，下面称为 CONFIG。认证目录是宿主的 auth-dir，常见是 ~/.cli-proxy-api。

1. 编译动态库，放到宿主加载插件的目录。文件名就是插件 id。

        CGO_ENABLED=1 go build -buildmode=c-shared -o z-coding-plan.so .

   Linux amd64 的目标路径是 CONFIG/plugins/linux/amd64/z-coding-plan.so。

   完成标准：该路径存在，且文件名正好是 z-coding-plan.so。

2. 在 CONFIG/config.yaml 里启用插件。宿主在这里只读 enabled 和 priority，z-coding-plan 下面不要加别的字段。

        plugins:
          enabled: true
          configs:
            z-coding-plan:
              enabled: true
              priority: 1

   完成标准：plugins.enabled 为 true，且 plugins.configs.z-coding-plan.enabled 为 true。

3. 每个智谱 Coding Plan Key 在认证目录里放一个文件，权限 0600。一个文件一把 Key。

        {
          "type": "z-coding-plan",
          "label": "Key 1",
          "api_key": "智谱上游 Key"
        }

   type 必须是字符串 z-coding-plan。api_key 必填，填智谱上游 Key，不要填 CLIProxyAPI 的客户端访问密钥。api-key 也可以当作别名。label 可省略，省略时用去掉 .json 的文件名。宿主看到的凭证 id 是去掉 .json 的文件名。

   完成标准：每个文件都是合法 JSON，api_key 非空，type 正好是 z-coding-plan。

4. 请求路由仍放在宿主已有的上游 Key 列表里。本插件不复制这些 Key，也不会自己发模型请求。额度认证文件不会变成路由 Key。

   完成标准：要承接 glm-5.3 流量的同一批智谱 Key，仍然留在宿主的上游配置里。

5. 重启 CLIProxyAPI。替换 .so 不会影响已经加载它的进程。

   完成标准：日志里出现 plugin registered plugin_id=z-coding-plan。

6. 用管理密钥检查管理接口。

   - GET /v0/management/plugins 里有 z-coding-plan，registered 为 true，supports_quota 为 true。
   - GET /v0/management/auth-files 列出每个新文件，provider 为 z-coding-plan，supports_quota 为 true，并带 auth_index。
   - POST /v8/management/plugins/z-coding-plan/quota，请求体为 {"auth_index":"<auth_index>"}，返回套餐名和窗口。Token 窗口标为额度，积分窗口标为积分，MCP 标为 MCP。

   完成标准：每个额度认证文件都返回窗口，或者返回的错误不是 quota credential not found。

配置成功后，管理页的额度页会列出每把中国区 Coding Plan Key。卡片上的套餐名来自接口，5 小时和 MCP 是两个窗口。数字会随用量变化，下面只是一次成功配置的样子。

![额度页：两把中国区智谱 Coding Plan Key](docs/quota-page.png)

## 1308 切换

所有路由凭证都正常时，插件不参与选 Key。

请求失败后，只有错误文本里的 1308 是独立数字，并且带有 YYYY-MM-DD HH:MM:SS 时间戳，才会停用那把凭证。时间按中国时间（UTC+8）读取，停用到该时刻。130812 不算。只有 1308、没有时间戳，也不停用。其他错误不停用。

下一次选择会跳过已停用的凭证 id。如果候选全部停用，就用重置时间最早的那把。停用名单只在内存里，进程退出或插件卸载后清空。

被停用的 id 是实际发出请求的宿主凭证 id，不是额度文件名，除非宿主路由用的就是那个文件。

## 发布

发布 tag 为 v0.1.0。附上 checksums.txt，以及每个平台一个 zip。zip 根目录只放动态库，例如 z-coding-plan_0.1.0_linux_amd64.zip 里直接是 z-coding-plan.so。

登记字段：id 为 z-coding-plan，name 为 Z Coding Plan，author 为 iamdin，repository 为 https://github.com/iamdin/cliproxyapi-plugin-z-coding-plan，license 为 MIT。
