
// this file is generated — do not edit it


/// <reference types="@sveltejs/kit" />

/**
 * This module provides access to environment variables that are injected _statically_ into your bundle at build time and are limited to _private_ access.
 * 
 * |         | Runtime                                                                    | Build time                                                               |
 * | ------- | -------------------------------------------------------------------------- | ------------------------------------------------------------------------ |
 * | Private | [`$env/dynamic/private`](https://svelte.dev/docs/kit/$env-dynamic-private) | [`$env/static/private`](https://svelte.dev/docs/kit/$env-static-private) |
 * | Public  | [`$env/dynamic/public`](https://svelte.dev/docs/kit/$env-dynamic-public)   | [`$env/static/public`](https://svelte.dev/docs/kit/$env-static-public)   |
 * 
 * Static environment variables are [loaded by Vite](https://vitejs.dev/guide/env-and-mode.html#env-files) from `.env` files and `process.env` at build time and then statically injected into your bundle at build time, enabling optimisations like dead code elimination.
 * 
 * **_Private_ access:**
 * 
 * - This module cannot be imported into client-side code
 * - This module only includes variables that _do not_ begin with [`config.kit.env.publicPrefix`](https://svelte.dev/docs/kit/configuration#env) _and do_ start with [`config.kit.env.privatePrefix`](https://svelte.dev/docs/kit/configuration#env) (if configured)
 * 
 * For example, given the following build time environment:
 * 
 * ```env
 * ENVIRONMENT=production
 * PUBLIC_BASE_URL=http://site.com
 * ```
 * 
 * With the default `publicPrefix` and `privatePrefix`:
 * 
 * ```ts
 * import { ENVIRONMENT, PUBLIC_BASE_URL } from '$env/static/private';
 * 
 * console.log(ENVIRONMENT); // => "production"
 * console.log(PUBLIC_BASE_URL); // => throws error during build
 * ```
 * 
 * The above values will be the same _even if_ different values for `ENVIRONMENT` or `PUBLIC_BASE_URL` are set at runtime, as they are statically replaced in your code with their build time values.
 */
declare module '$env/static/private' {
	export const SHELL: string;
	export const npm_command: string;
	export const COREPACK_ENABLE_AUTO_PIN: string;
	export const SESSION_MANAGER: string;
	export const npm_config_userconfig: string;
	export const XDG_CONFIG_DIRS: string;
	export const npm_config_cache: string;
	export const CLAUDE_CODE_CHILD_SESSION: string;
	export const CLAUDE_CODE_EMIT_TOOL_USE_SUMMARIES: string;
	export const CLAUDE_PREVIEW_CLASSIFIER_FLOOR: string;
	export const XDG_SESSION_PATH: string;
	export const CLAUDE_CODE_ENABLE_ASK_USER_QUESTION_TOOL: string;
	export const XDG_MENU_PREFIX: string;
	export const FNM_ARCH: string;
	export const AI_AGENT: string;
	export const ICEAUTHORITY: string;
	export const CLAUDE_CODE_SESSION_ID: string;
	export const NODE: string;
	export const LC_ADDRESS: string;
	export const CLAUDE_PID: string;
	export const LC_NAME: string;
	export const SSH_AUTH_SOCK: string;
	export const CLAUDE_EFFORT: string;
	export const GOPRIVATE: string;
	export const MEMORY_PRESSURE_WRITE: string;
	export const FNM_NODE_DIST_MIRROR: string;
	export const COLOR: string;
	export const CLAUDE_CODE_REPORT_FINDINGS: string;
	export const npm_config_local_prefix: string;
	export const DESKTOP_SESSION: string;
	export const LC_MONETARY: string;
	export const SSH_AGENT_PID: string;
	export const GTK_RC_FILES: string;
	export const NO_AT_BRIDGE: string;
	export const npm_config_globalconfig: string;
	export const EDITOR: string;
	export const CLAUDE_CODE_SDK_HAS_HOST_AUTH_REFRESH: string;
	export const XDG_SEAT: string;
	export const PWD: string;
	export const CLAUDE_CODE_EAGER_FLUSH: string;
	export const XDG_SESSION_DESKTOP: string;
	export const LOGNAME: string;
	export const XDG_SESSION_TYPE: string;
	export const PNPM_HOME: string;
	export const npm_config_init_module: string;
	export const SYSTEMD_EXEC_PID: string;
	export const _: string;
	export const XAUTHORITY: string;
	export const NoDefaultCurrentDirectoryInExePath: string;
	export const CLAUDECODE: string;
	export const MOTD_SHOWN: string;
	export const GTK2_RC_FILES: string;
	export const HOME: string;
	export const CLAUDE_AGENT_SDK_VERSION: string;
	export const LC_PAPER: string;
	export const LANG: string;
	export const FNM_COREPACK_ENABLED: string;
	export const _JAVA_AWT_WM_NONREPARENTING: string;
	export const XDG_CURRENT_DESKTOP: string;
	export const CLAUDE_CODE_HOST_SESSION_ID: string;
	export const MEMORY_PRESSURE_WATCH: string;
	export const CLOUDSDK_ROOT_DIR: string;
	export const WAYLAND_DISPLAY: string;
	export const VIRTUAL_ENV_DISABLE_PROMPT: string;
	export const CLAUDE_CODE_DISABLE_CRON: string;
	export const MANROFFOPT: string;
	export const XDG_SEAT_PATH: string;
	export const TESTCONTAINER_PROVIDER: string;
	export const INVOCATION_ID: string;
	export const ANTHROPIC_BASE_URL: string;
	export const CLAUDE_CODE_OAUTH_SCOPES: string;
	export const DOCKER_BUILDKIT: string;
	export const MANAGERPID: string;
	export const INIT_CWD: string;
	export const CHROME_DESKTOP: string;
	export const KDE_SESSION_UID: string;
	export const npm_lifecycle_script: string;
	export const XKB_DEFAULT_LAYOUT: string;
	export const COMPOSE_DOCKER_CLI_BUILD: string;
	export const npm_config_npm_version: string;
	export const XDG_SESSION_CLASS: string;
	export const LC_IDENTIFICATION: string;
	export const npm_config_prefix: string;
	export const GOOGLE_CLOUD_SDK_HOME: string;
	export const USER: string;
	export const CUDA_PATH: string;
	export const QT_WAYLAND_RECONNECT: string;
	export const KDE_SESSION_VERSION: string;
	export const PAM_KWALLET5_LOGIN: string;
	export const PODMAN_COMPOSE_PROVIDER: string;
	export const MANPAGER: string;
	export const DISPLAY: string;
	export const npm_lifecycle_event: string;
	export const SHLVL: string;
	export const GIT_EDITOR: string;
	export const LC_TELEPHONE: string;
	export const LC_MEASUREMENT: string;
	export const FNM_VERSION_FILE_STRATEGY: string;
	export const XDG_VTNR: string;
	export const XDG_SESSION_ID: string;
	export const MANAGERPIDFDID: string;
	export const USE_LOCAL_OAUTH: string;
	export const npm_config_user_agent: string;
	export const CUDA_DISABLE_PERF_BOOST: string;
	export const npm_execpath: string;
	export const FC_FONTATIONS: string;
	export const DISABLE_AUTOUPDATER: string;
	export const XDG_RUNTIME_DIR: string;
	export const FNM_RESOLVE_ENGINES: string;
	export const CLAUDE_CODE_ENTRYPOINT: string;
	export const NVCC_CCBIN: string;
	export const DEBUGINFOD_URLS: string;
	export const npm_package_json: string;
	export const NODE_USE_SYSTEM_CA: string;
	export const API_TIMEOUT_MS: string;
	export const DOCKER_HOST: string;
	export const LC_TIME: string;
	export const MCP_CONNECTION_NONBLOCKING: string;
	export const JOURNAL_STREAM: string;
	export const GDK_BACKEND: string;
	export const KDE_FULL_SESSION: string;
	export const CLAUDE_CODE_EXECPATH: string;
	export const npm_config_noproxy: string;
	export const PATH: string;
	export const npm_config_node_gyp: string;
	export const DBUS_SESSION_BUS_ADDRESS: string;
	export const npm_config_global_prefix: string;
	export const KDE_APPLICATIONS_AS_SCOPE: string;
	export const MAIL: string;
	export const FNM_DIR: string;
	export const FNM_MULTISHELL_PATH: string;
	export const BAGGAGE: string;
	export const npm_node_execpath: string;
	export const DISABLE_MICROCOMPACT: string;
	export const FNM_LOGLEVEL: string;
	export const LC_NUMERIC: string;
	export const OLDPWD: string;
	export const GOPATH: string;
	export const USE_STAGING_OAUTH: string;
	export const CLAUDE_CODE_SDK_HAS_OAUTH_REFRESH: string;
	export const TEST: string;
	export const VITEST: string;
	export const NODE_ENV: string;
	export const PROD: string;
	export const DEV: string;
	export const BASE_URL: string;
	export const MODE: string;
}

/**
 * This module provides access to environment variables that are injected _statically_ into your bundle at build time and are _publicly_ accessible.
 * 
 * |         | Runtime                                                                    | Build time                                                               |
 * | ------- | -------------------------------------------------------------------------- | ------------------------------------------------------------------------ |
 * | Private | [`$env/dynamic/private`](https://svelte.dev/docs/kit/$env-dynamic-private) | [`$env/static/private`](https://svelte.dev/docs/kit/$env-static-private) |
 * | Public  | [`$env/dynamic/public`](https://svelte.dev/docs/kit/$env-dynamic-public)   | [`$env/static/public`](https://svelte.dev/docs/kit/$env-static-public)   |
 * 
 * Static environment variables are [loaded by Vite](https://vitejs.dev/guide/env-and-mode.html#env-files) from `.env` files and `process.env` at build time and then statically injected into your bundle at build time, enabling optimisations like dead code elimination.
 * 
 * **_Public_ access:**
 * 
 * - This module _can_ be imported into client-side code
 * - **Only** variables that begin with [`config.kit.env.publicPrefix`](https://svelte.dev/docs/kit/configuration#env) (which defaults to `PUBLIC_`) are included
 * 
 * For example, given the following build time environment:
 * 
 * ```env
 * ENVIRONMENT=production
 * PUBLIC_BASE_URL=http://site.com
 * ```
 * 
 * With the default `publicPrefix` and `privatePrefix`:
 * 
 * ```ts
 * import { ENVIRONMENT, PUBLIC_BASE_URL } from '$env/static/public';
 * 
 * console.log(ENVIRONMENT); // => throws error during build
 * console.log(PUBLIC_BASE_URL); // => "http://site.com"
 * ```
 * 
 * The above values will be the same _even if_ different values for `ENVIRONMENT` or `PUBLIC_BASE_URL` are set at runtime, as they are statically replaced in your code with their build time values.
 */
declare module '$env/static/public' {
	
}

/**
 * This module provides access to environment variables set _dynamically_ at runtime and that are limited to _private_ access.
 * 
 * |         | Runtime                                                                    | Build time                                                               |
 * | ------- | -------------------------------------------------------------------------- | ------------------------------------------------------------------------ |
 * | Private | [`$env/dynamic/private`](https://svelte.dev/docs/kit/$env-dynamic-private) | [`$env/static/private`](https://svelte.dev/docs/kit/$env-static-private) |
 * | Public  | [`$env/dynamic/public`](https://svelte.dev/docs/kit/$env-dynamic-public)   | [`$env/static/public`](https://svelte.dev/docs/kit/$env-static-public)   |
 * 
 * Dynamic environment variables are defined by the platform you're running on. For example if you're using [`adapter-node`](https://github.com/sveltejs/kit/tree/main/packages/adapter-node) (or running [`vite preview`](https://svelte.dev/docs/kit/cli)), this is equivalent to `process.env`.
 * 
 * **_Private_ access:**
 * 
 * - This module cannot be imported into client-side code
 * - This module includes variables that _do not_ begin with [`config.kit.env.publicPrefix`](https://svelte.dev/docs/kit/configuration#env) _and do_ start with [`config.kit.env.privatePrefix`](https://svelte.dev/docs/kit/configuration#env) (if configured)
 * 
 * > [!NOTE] In `dev`, `$env/dynamic` includes environment variables from `.env`. In `prod`, this behavior will depend on your adapter.
 * 
 * > [!NOTE] To get correct types, environment variables referenced in your code should be declared (for example in an `.env` file), even if they don't have a value until the app is deployed:
 * >
 * > ```env
 * > MY_FEATURE_FLAG=
 * > ```
 * >
 * > You can override `.env` values from the command line like so:
 * >
 * > ```sh
 * > MY_FEATURE_FLAG="enabled" npm run dev
 * > ```
 * 
 * For example, given the following runtime environment:
 * 
 * ```env
 * ENVIRONMENT=production
 * PUBLIC_BASE_URL=http://site.com
 * ```
 * 
 * With the default `publicPrefix` and `privatePrefix`:
 * 
 * ```ts
 * import { env } from '$env/dynamic/private';
 * 
 * console.log(env.ENVIRONMENT); // => "production"
 * console.log(env.PUBLIC_BASE_URL); // => undefined
 * ```
 */
declare module '$env/dynamic/private' {
	export const env: {
		SHELL: string;
		npm_command: string;
		COREPACK_ENABLE_AUTO_PIN: string;
		SESSION_MANAGER: string;
		npm_config_userconfig: string;
		XDG_CONFIG_DIRS: string;
		npm_config_cache: string;
		CLAUDE_CODE_CHILD_SESSION: string;
		CLAUDE_CODE_EMIT_TOOL_USE_SUMMARIES: string;
		CLAUDE_PREVIEW_CLASSIFIER_FLOOR: string;
		XDG_SESSION_PATH: string;
		CLAUDE_CODE_ENABLE_ASK_USER_QUESTION_TOOL: string;
		XDG_MENU_PREFIX: string;
		FNM_ARCH: string;
		AI_AGENT: string;
		ICEAUTHORITY: string;
		CLAUDE_CODE_SESSION_ID: string;
		NODE: string;
		LC_ADDRESS: string;
		CLAUDE_PID: string;
		LC_NAME: string;
		SSH_AUTH_SOCK: string;
		CLAUDE_EFFORT: string;
		GOPRIVATE: string;
		MEMORY_PRESSURE_WRITE: string;
		FNM_NODE_DIST_MIRROR: string;
		COLOR: string;
		CLAUDE_CODE_REPORT_FINDINGS: string;
		npm_config_local_prefix: string;
		DESKTOP_SESSION: string;
		LC_MONETARY: string;
		SSH_AGENT_PID: string;
		GTK_RC_FILES: string;
		NO_AT_BRIDGE: string;
		npm_config_globalconfig: string;
		EDITOR: string;
		CLAUDE_CODE_SDK_HAS_HOST_AUTH_REFRESH: string;
		XDG_SEAT: string;
		PWD: string;
		CLAUDE_CODE_EAGER_FLUSH: string;
		XDG_SESSION_DESKTOP: string;
		LOGNAME: string;
		XDG_SESSION_TYPE: string;
		PNPM_HOME: string;
		npm_config_init_module: string;
		SYSTEMD_EXEC_PID: string;
		_: string;
		XAUTHORITY: string;
		NoDefaultCurrentDirectoryInExePath: string;
		CLAUDECODE: string;
		MOTD_SHOWN: string;
		GTK2_RC_FILES: string;
		HOME: string;
		CLAUDE_AGENT_SDK_VERSION: string;
		LC_PAPER: string;
		LANG: string;
		FNM_COREPACK_ENABLED: string;
		_JAVA_AWT_WM_NONREPARENTING: string;
		XDG_CURRENT_DESKTOP: string;
		CLAUDE_CODE_HOST_SESSION_ID: string;
		MEMORY_PRESSURE_WATCH: string;
		CLOUDSDK_ROOT_DIR: string;
		WAYLAND_DISPLAY: string;
		VIRTUAL_ENV_DISABLE_PROMPT: string;
		CLAUDE_CODE_DISABLE_CRON: string;
		MANROFFOPT: string;
		XDG_SEAT_PATH: string;
		TESTCONTAINER_PROVIDER: string;
		INVOCATION_ID: string;
		ANTHROPIC_BASE_URL: string;
		CLAUDE_CODE_OAUTH_SCOPES: string;
		DOCKER_BUILDKIT: string;
		MANAGERPID: string;
		INIT_CWD: string;
		CHROME_DESKTOP: string;
		KDE_SESSION_UID: string;
		npm_lifecycle_script: string;
		XKB_DEFAULT_LAYOUT: string;
		COMPOSE_DOCKER_CLI_BUILD: string;
		npm_config_npm_version: string;
		XDG_SESSION_CLASS: string;
		LC_IDENTIFICATION: string;
		npm_config_prefix: string;
		GOOGLE_CLOUD_SDK_HOME: string;
		USER: string;
		CUDA_PATH: string;
		QT_WAYLAND_RECONNECT: string;
		KDE_SESSION_VERSION: string;
		PAM_KWALLET5_LOGIN: string;
		PODMAN_COMPOSE_PROVIDER: string;
		MANPAGER: string;
		DISPLAY: string;
		npm_lifecycle_event: string;
		SHLVL: string;
		GIT_EDITOR: string;
		LC_TELEPHONE: string;
		LC_MEASUREMENT: string;
		FNM_VERSION_FILE_STRATEGY: string;
		XDG_VTNR: string;
		XDG_SESSION_ID: string;
		MANAGERPIDFDID: string;
		USE_LOCAL_OAUTH: string;
		npm_config_user_agent: string;
		CUDA_DISABLE_PERF_BOOST: string;
		npm_execpath: string;
		FC_FONTATIONS: string;
		DISABLE_AUTOUPDATER: string;
		XDG_RUNTIME_DIR: string;
		FNM_RESOLVE_ENGINES: string;
		CLAUDE_CODE_ENTRYPOINT: string;
		NVCC_CCBIN: string;
		DEBUGINFOD_URLS: string;
		npm_package_json: string;
		NODE_USE_SYSTEM_CA: string;
		API_TIMEOUT_MS: string;
		DOCKER_HOST: string;
		LC_TIME: string;
		MCP_CONNECTION_NONBLOCKING: string;
		JOURNAL_STREAM: string;
		GDK_BACKEND: string;
		KDE_FULL_SESSION: string;
		CLAUDE_CODE_EXECPATH: string;
		npm_config_noproxy: string;
		PATH: string;
		npm_config_node_gyp: string;
		DBUS_SESSION_BUS_ADDRESS: string;
		npm_config_global_prefix: string;
		KDE_APPLICATIONS_AS_SCOPE: string;
		MAIL: string;
		FNM_DIR: string;
		FNM_MULTISHELL_PATH: string;
		BAGGAGE: string;
		npm_node_execpath: string;
		DISABLE_MICROCOMPACT: string;
		FNM_LOGLEVEL: string;
		LC_NUMERIC: string;
		OLDPWD: string;
		GOPATH: string;
		USE_STAGING_OAUTH: string;
		CLAUDE_CODE_SDK_HAS_OAUTH_REFRESH: string;
		TEST: string;
		VITEST: string;
		NODE_ENV: string;
		PROD: string;
		DEV: string;
		BASE_URL: string;
		MODE: string;
		[key: `PUBLIC_${string}`]: undefined;
		[key: `${string}`]: string | undefined;
	}
}

/**
 * This module provides access to environment variables set _dynamically_ at runtime and that are _publicly_ accessible.
 * 
 * |         | Runtime                                                                    | Build time                                                               |
 * | ------- | -------------------------------------------------------------------------- | ------------------------------------------------------------------------ |
 * | Private | [`$env/dynamic/private`](https://svelte.dev/docs/kit/$env-dynamic-private) | [`$env/static/private`](https://svelte.dev/docs/kit/$env-static-private) |
 * | Public  | [`$env/dynamic/public`](https://svelte.dev/docs/kit/$env-dynamic-public)   | [`$env/static/public`](https://svelte.dev/docs/kit/$env-static-public)   |
 * 
 * Dynamic environment variables are defined by the platform you're running on. For example if you're using [`adapter-node`](https://github.com/sveltejs/kit/tree/main/packages/adapter-node) (or running [`vite preview`](https://svelte.dev/docs/kit/cli)), this is equivalent to `process.env`.
 * 
 * **_Public_ access:**
 * 
 * - This module _can_ be imported into client-side code
 * - **Only** variables that begin with [`config.kit.env.publicPrefix`](https://svelte.dev/docs/kit/configuration#env) (which defaults to `PUBLIC_`) are included
 * 
 * > [!NOTE] In `dev`, `$env/dynamic` includes environment variables from `.env`. In `prod`, this behavior will depend on your adapter.
 * 
 * > [!NOTE] To get correct types, environment variables referenced in your code should be declared (for example in an `.env` file), even if they don't have a value until the app is deployed:
 * >
 * > ```env
 * > MY_FEATURE_FLAG=
 * > ```
 * >
 * > You can override `.env` values from the command line like so:
 * >
 * > ```sh
 * > MY_FEATURE_FLAG="enabled" npm run dev
 * > ```
 * 
 * For example, given the following runtime environment:
 * 
 * ```env
 * ENVIRONMENT=production
 * PUBLIC_BASE_URL=http://example.com
 * ```
 * 
 * With the default `publicPrefix` and `privatePrefix`:
 * 
 * ```ts
 * import { env } from '$env/dynamic/public';
 * console.log(env.ENVIRONMENT); // => undefined, not public
 * console.log(env.PUBLIC_BASE_URL); // => "http://example.com"
 * ```
 * 
 * ```
 * 
 * ```
 */
declare module '$env/dynamic/public' {
	export const env: {
		[key: `PUBLIC_${string}`]: string | undefined;
	}
}
