# z-coding-plan

CLIProxyAPI plugin. It shows Zhipu GLM Coding Plan quota, and skips a routed credential after Zhipu error 1308 until the reset time in that error.

Quota is always read from https://bigmodel.cn/api/monitor/usage/quota/limit. There is no endpoint setting.

Two different credentials are involved. Keep them separate:

- A quota auth file is a JSON file the plugin parses. It exists so the management quota page can show remaining quota. It is not a routing key.
- A routing credential is the upstream API key CLIProxyAPI already uses to call Zhipu. Failover follows that credential's id, the one that actually served the failed request.

## Configure

Work in the CLIProxyAPI config directory. Call it CONFIG. The auth directory is the host auth-dir (often ~/.cli-proxy-api).

1. Build the library and put it where the host loads plugins. The file name is the plugin id.

        CGO_ENABLED=1 go build -buildmode=c-shared -o z-coding-plan.so .

   Linux amd64 destination: CONFIG/plugins/linux/amd64/z-coding-plan.so.

   Done when that path exists and the file name is exactly z-coding-plan.so.

2. Enable the plugin in CONFIG/config.yaml. The host only reads enabled and priority here. Add nothing else under z-coding-plan.

        plugins:
          enabled: true
          configs:
            z-coding-plan:
              enabled: true
              priority: 1

   Done when plugins.enabled is true and plugins.configs.z-coding-plan.enabled is true.

3. For each Zhipu Coding Plan key, write one auth file in the auth directory. Mode 0600. One key per file.

        {
          "type": "z-coding-plan",
          "label": "Key 1",
          "api_key": "THE_ZHIPU_KEY"
        }

   type must be the string z-coding-plan. api_key is required and is the Zhipu upstream key, not the CLIProxyAPI client access key. api-key is accepted as an alias. label is optional; if omitted, the file name without .json is the label. The credential id shown to the host is the file name without .json.

   Done when each file is valid JSON, contains a non-empty api_key, and type is exactly z-coding-plan.

4. Leave request routing in the host's existing upstream key list. This plugin does not copy those keys and does not send traffic by itself. A quota auth file does not become a routing key.

   Done when the same Zhipu keys that should serve glm-5.3 traffic are still present in the host upstream configuration.

5. Restart CLIProxyAPI. Replacing the .so does not affect the process that already loaded it.

   Done when the log contains: plugin registered plugin_id=z-coding-plan.

6. Check the management API with the management key.

   - GET /v0/management/plugins includes z-coding-plan with registered true and supports_quota true.
   - GET /v0/management/auth-files lists each new file with provider z-coding-plan, supports_quota true, and an auth_index.
   - POST /v8/management/plugins/z-coding-plan/quota with body {"auth_index":"<auth_index>"} returns a plan name and windows. Token windows are labeled 额度. Credit windows are labeled 积分. MCP is labeled MCP.

   Done when every quota auth file returns windows, or a quota error that is not "quota credential not found".

## Failover

The plugin does not choose a key while every routed credential is healthy.

After a request fails, it blocks that credential only when the error text has 1308 as its own number and a timestamp YYYY-MM-DD HH:MM:SS. The timestamp is read as China time (UTC+8). The block lasts until that time. 130812 does not match. 1308 with no timestamp does not block. Any other error does not block.

The next selection skips blocked credential ids. If every candidate is blocked, the one that resets soonest is used. The block list is in memory and is cleared when the process exits or the plugin is unloaded.

The blocked id is the host id of the credential that served the request. It is not the quota file name unless that file is the credential the host routed.

## Publish

Tag the release v0.1.0. Attach checksums.txt and one zip per platform. The zip root contains only the library, for example z-coding-plan.so inside z-coding-plan_0.1.0_linux_amd64.zip.

Registry fields: id z-coding-plan, name Z Coding Plan, author iamdin, repository https://github.com/iamdin/cliproxyapi-plugin-z-coding-plan, license MIT.
