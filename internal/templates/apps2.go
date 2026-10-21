package templates

import "github.com/thesunbg/paas/internal/model"

// Second batch: search engines, AI agents and more popular selfh.st apps.
func init() { All = append(All, apps2...) }

var apps2 = []Template{
	// Search and data stores
	{
		ID: "elasticsearch", Title: "Elasticsearch", Category: "Search", Website: "https://www.elastic.co/elasticsearch",
		Description: "Search and analytics engine (single node, security on). Private: services of the project reach it at http://elasticsearch:9200 as user elastic.",
		Services: []Service{{
			Name: "elasticsearch", Image: "docker.io/library/elasticsearch:9.1.5", Type: "web", Port: 9200, Size: "large", Private: true,
			Env: map[string]string{
				"ES_SETTING_DISCOVERY_TYPE": "single-node", "ES_JAVA_OPTS": "-Xms1g -Xmx1g", "ES_SETTING_XPACK_SECURITY_ENABLED": "true",
				"ES_SETTING_XPACK_SECURITY_HTTP_SSL_ENABLED": "false", "ES_SETTING_XPACK_SECURITY_TRANSPORT_SSL_ENABLED": "false",
			},
			Inputs:  []EnvInput{secretInput("ELASTIC_PASSWORD", "Password of the elastic superuser")},
			Volumes: []model.Volume{vol("data", "/usr/share/elasticsearch/data")},
		}},
	},
	{
		ID: "opensearch", Title: "OpenSearch", Category: "Search", Website: "https://opensearch.org",
		Description: "Open source fork of Elasticsearch (single node). Private, without the security plugin: only services of the project reach it, at http://opensearch:9200.",
		Services: []Service{{
			Name: "opensearch", Image: "docker.io/opensearchproject/opensearch:3", Type: "web", Port: 9200, Size: "large", Private: true,
			// discovery.type cannot be an environment variable here (no dots in names): pass it as a setting.
			Command: []string{"./opensearch-docker-entrypoint.sh", "opensearch", "-Ediscovery.type=single-node"},
			Env:     map[string]string{"OPENSEARCH_JAVA_OPTS": "-Xms1g -Xmx1g", "DISABLE_SECURITY_PLUGIN": "true", "DISABLE_INSTALL_DEMO_CONFIG": "true"},
			Volumes: []model.Volume{vol("data", "/usr/share/opensearch/data")},
		}},
	},
	{
		ID: "typesense", Title: "Typesense", Category: "Search", Website: "https://typesense.org",
		Description: "Fast, typo-tolerant search API. The admin API key is TYPESENSE_API_KEY in the service's Environment.",
		Services: []Service{{
			Name: "typesense", Image: "docker.io/typesense/typesense:29.0", Type: "web", Port: 8108, Size: "small",
			Env:     map[string]string{"TYPESENSE_API_KEY": "${random}", "TYPESENSE_DATA_DIR": "/data", "TYPESENSE_ENABLE_CORS": "true"},
			Volumes: []model.Volume{vol("data", "/data")},
			Health:  "/health",
		}},
	},
	{
		ID: "rabbitmq", Title: "RabbitMQ", Category: "Databases", Website: "https://www.rabbitmq.com",
		Description: "Message broker with the management UI. Services of the project connect to amqp://admin:<password>@rabbitmq:5672.",
		Services: []Service{{
			Name: "rabbitmq", Image: "docker.io/library/rabbitmq:4-management", Type: "web", Port: 15672, Size: "small",
			Env:     map[string]string{"RABBITMQ_DEFAULT_USER": "admin"},
			Inputs:  []EnvInput{secretInput("RABBITMQ_DEFAULT_PASS", "Password of the admin user")},
			Volumes: []model.Volume{vol("data", "/var/lib/rabbitmq")},
		}},
	},
	{
		ID: "influxdb", Title: "InfluxDB", Category: "Databases", Website: "https://www.influxdata.com",
		Description: "Time series database for metrics and events, with its web UI.",
		Services: []Service{{
			Name: "influxdb", Image: "docker.io/library/influxdb:2", Type: "web", Port: 8086, Size: "small",
			Env: map[string]string{
				"DOCKER_INFLUXDB_INIT_MODE": "setup", "DOCKER_INFLUXDB_INIT_USERNAME": "admin", "DOCKER_INFLUXDB_INIT_ORG": "main",
				"DOCKER_INFLUXDB_INIT_BUCKET": "default", "DOCKER_INFLUXDB_INIT_ADMIN_TOKEN": "${random}",
			},
			Inputs:  []EnvInput{secretInput("DOCKER_INFLUXDB_INIT_PASSWORD", "Password of the admin user (8 characters or more)")},
			Volumes: []model.Volume{vol("data", "/var/lib/influxdb2"), vol("config", "/etc/influxdb2")},
			Health:  "/health",
		}},
	},

	// AI agents and models
	{
		ID: "openclaw", Title: "OpenClaw", Category: "AI", Website: "https://openclaw.ai",
		Description: "Personal AI assistant gateway with a web Control UI. Sign in with the gateway token (OPENCLAW_GATEWAY_TOKEN in Environment); add model providers and channels from the UI or the Console (node dist/index.js …).",
		Services: []Service{{
			Name: "openclaw", Image: "ghcr.io/openclaw/openclaw:latest", Type: "web", Port: 18789, Size: "large",
			// The gateway refuses to start without gateway.mode, and the Control UI only accepts
			// its own public origin; both are set on every start, the rest of the config is kept.
			Command: []string{`node dist/index.js config set --batch-json "[{\"path\":\"gateway.mode\",\"value\":\"local\"},{\"path\":\"gateway.bind\",\"value\":\"lan\"},{\"path\":\"gateway.auth.mode\",\"value\":\"token\"},{\"path\":\"gateway.trustedProxies\",\"value\":[\"10.0.0.0/8\"]},{\"path\":\"gateway.controlUi.allowedOrigins\",\"value\":[\"$OPENCLAW_PUBLIC_URL\"]}]" && exec node dist/index.js gateway --bind lan --port 18789`},
			Env: map[string]string{
				"HOME": "/home/node", "OPENCLAW_HOME": "/home/node", "OPENCLAW_STATE_DIR": "/home/node/.openclaw",
				"OPENCLAW_CONFIG_PATH": "/home/node/.openclaw/openclaw.json", "OPENCLAW_WORKSPACE_DIR": "/home/node/.openclaw/workspace",
				"OPENCLAW_GATEWAY_PORT": "18789", "OPENCLAW_GATEWAY_TOKEN": "${random}", "OPENCLAW_PUBLIC_URL": "${url}", "TZ": "UTC",
			},
			Inputs: []EnvInput{
				{Key: "ANTHROPIC_API_KEY", Hint: "Optional: Anthropic API key for Claude models"},
				{Key: "OPENAI_API_KEY", Hint: "Optional: OpenAI API key"},
				{Key: "TELEGRAM_BOT_TOKEN", Hint: "Optional: Telegram bot token, to chat with it from Telegram"},
			},
			Volumes: []model.Volume{vol("state", "/home/node/.openclaw"), vol("auth", "/home/node/.config/openclaw")},
		}},
	},
	{
		ID: "tinyagi", Title: "TinyClaw (TinyAGI)", Category: "AI", Website: "https://github.com/TinyAGI/tinyclaw",
		Description: "Multi-agent AI assistant that runs 24/7 behind Telegram, Discord or WhatsApp. Built from its repository and kept private (its API has no login): set up channels with tinyagi channel setup in the Console.",
		Repo:        "https://github.com/TinyAGI/tinyclaw", Branch: "main",
		Services: []Service{{
			Name: "tinyagi", Type: "worker", Size: "medium", Private: true,
			Build: &Build{RootDir: ""},
			Env:   map[string]string{"TINYAGI_SKIP_UPDATE_CHECK": "1"},
			Inputs: []EnvInput{
				{Key: "ANTHROPIC_API_KEY", Hint: "Anthropic API key (the default agent uses Claude)"},
				{Key: "OPENAI_API_KEY", Hint: "Optional: OpenAI API key"},
				{Key: "TELEGRAM_BOT_TOKEN", Hint: "Optional: Telegram bot token"},
				{Key: "DISCORD_BOT_TOKEN", Hint: "Optional: Discord bot token"},
			},
			Volumes: []model.Volume{vol("data", "/root")},
		}},
	},
	{
		ID: "ollama", Title: "Ollama", Category: "AI", Website: "https://ollama.com",
		Description: "Run open models (Llama, Qwen, Gemma…) on the CPU. Private: point Open WebUI or your apps at http://ollama:11434, pull models from the Console (ollama pull qwen3:4b).",
		Services: []Service{{
			Name: "ollama", Image: "docker.io/ollama/ollama:latest", Type: "web", Port: 11434, Size: "xlarge", Private: true,
			Env:     map[string]string{"OLLAMA_HOST": "0.0.0.0:11434"},
			Volumes: []model.Volume{vol("models", "/root/.ollama")},
		}},
	},
	{
		ID: "jupyter", Title: "Jupyter", Category: "Development", Website: "https://jupyter.org",
		Description: "JupyterLab notebooks in the browser; notebooks are kept in the work folder.",
		Services: []Service{{
			Name: "jupyter", Image: "quay.io/jupyter/minimal-notebook:latest", Type: "web", Port: 8888, Size: "medium",
			Inputs:  []EnvInput{secretInput("JUPYTER_TOKEN", "Token (password) to open JupyterLab")},
			Volumes: []model.Volume{vol("work", "/home/jovyan/work")},
		}},
	},

	// Automation and productivity
	{
		ID: "activepieces", Title: "Activepieces", Category: "Automation", Website: "https://www.activepieces.com",
		Description: "No-code automation with hundreds of integrations (Zapier alternative).",
		Databases:   []Database{{Name: "activepieces", Engine: "postgres"}, {Name: "redis", Engine: "redis", Mode: "dedicated"}},
		Services: []Service{{
			Name: "activepieces", Image: "docker.io/activepieces/activepieces:latest", Type: "web", Port: 80, Size: "medium",
			Env: map[string]string{
				"AP_ENGINE_EXECUTABLE_PATH": "dist/packages/engine/main.js", "AP_ENVIRONMENT": "prod", "AP_FRONTEND_URL": "${url}",
				"AP_ENCRYPTION_KEY": "${random:32}", "AP_JWT_SECRET": "${random}", "AP_EXECUTION_MODE": "UNSANDBOXED",
				"AP_POSTGRES_HOST": "${db.activepieces.host}", "AP_POSTGRES_PORT": "${db.activepieces.port}",
				"AP_POSTGRES_DATABASE": "${db.activepieces.name}", "AP_POSTGRES_USERNAME": "${db.activepieces.user}",
				"AP_POSTGRES_PASSWORD": "${db.activepieces.password}", "AP_REDIS_URL": "${db.redis.url}", "AP_TELEMETRY_ENABLED": "false",
			},
		}},
	},
	{
		ID: "docmost", Title: "Docmost", Category: "Notes", Website: "https://docmost.com",
		Description: "Collaborative wiki and documentation (Notion/Confluence alternative).",
		Databases:   []Database{{Name: "docmost", Engine: "postgres"}, {Name: "redis", Engine: "redis", Mode: "dedicated"}},
		Services: []Service{{
			Name: "docmost", Image: "docker.io/docmost/docmost:latest", Type: "web", Port: 3000, Size: "small",
			Env: map[string]string{
				"APP_URL": "${url}", "APP_SECRET": "${random}", "DATABASE_URL": "${db.docmost.url}", "REDIS_URL": "${db.redis.url}",
			},
			Volumes: []model.Volume{vol("storage", "/app/data/storage")},
		}},
	},
	{
		ID: "planka", Title: "Planka", Category: "Productivity", Website: "https://planka.app",
		Description: "Real-time Kanban boards (Trello alternative).",
		Databases:   []Database{{Name: "planka", Engine: "postgres"}},
		Services: []Service{{
			Name: "planka", Image: "ghcr.io/plankanban/planka:latest", Type: "web", Port: 1337, Size: "small",
			Env: map[string]string{
				"BASE_URL": "${url}", "DATABASE_URL": "${db.planka.url}", "SECRET_KEY": "${random}", "TRUST_PROXY": "true",
				"DEFAULT_ADMIN_USERNAME": "admin", "DEFAULT_ADMIN_NAME": "Admin",
			},
			Inputs: []EnvInput{
				{Key: "DEFAULT_ADMIN_EMAIL", Required: true, Placeholder: "you@example.com", Hint: "Email of the admin account"},
				secretInput("DEFAULT_ADMIN_PASSWORD", "Password of the admin account"),
			},
			Volumes: []model.Volume{vol("data", "/app/data")},
		}},
	},
	{
		ID: "kanboard", Title: "Kanboard", Category: "Productivity", Website: "https://kanboard.org",
		Description: "Minimalist Kanban project management. Sign in with admin / admin and change the password.",
		Services: []Service{{
			Name: "kanboard", Image: "docker.io/kanboard/kanboard:latest", Type: "web", Port: 80, Size: "micro",
			Volumes: []model.Volume{vol("data", "/var/www/app/data"), vol("plugins", "/var/www/app/plugins")},
		}},
	},
	{
		ID: "etherpad", Title: "Etherpad", Category: "Productivity", Website: "https://etherpad.org",
		Description: "Real-time collaborative text editor.",
		Databases:   []Database{{Name: "etherpad", Engine: "postgres"}},
		Services: []Service{{
			Name: "etherpad", Image: "docker.io/etherpad/etherpad:latest", Type: "web", Port: 9001, Size: "small",
			Env: map[string]string{
				"DB_TYPE": "postgres", "DB_HOST": "${db.etherpad.host}", "DB_PORT": "${db.etherpad.port}", "DB_NAME": "${db.etherpad.name}",
				"DB_USER": "${db.etherpad.user}", "DB_PASS": "${db.etherpad.password}", "TRUST_PROXY": "true",
			},
			Inputs: []EnvInput{secretInput("ADMIN_PASSWORD", "Password of the admin page (/admin, user admin)")},
		}},
	},
	{
		ID: "grist", Title: "Grist", Category: "Databases", Website: "https://www.getgrist.com",
		Description: "Relational spreadsheets: a spreadsheet with the power of a database.",
		Services: []Service{{
			Name: "grist", Image: "docker.io/gristlabs/grist:latest", Type: "web", Port: 8484, Size: "small",
			Env:     map[string]string{"APP_HOME_URL": "${url}", "GRIST_SESSION_SECRET": "${random}", "GRIST_FORCE_LOGIN": "true"},
			Inputs:  []EnvInput{{Key: "GRIST_DEFAULT_EMAIL", Required: true, Placeholder: "you@example.com", Hint: "Email of the owner (administrator)"}},
			Volumes: []model.Volume{vol("data", "/persist")},
		}},
	},
	{
		ID: "baserow", Title: "Baserow", Category: "Databases", Website: "https://baserow.io",
		Description: "No-code database and app builder (Airtable alternative), all in one container.",
		Services: []Service{{
			Name: "baserow", Image: "docker.io/baserow/baserow:latest", Type: "web", Port: 80, Size: "large",
			Env:     map[string]string{"BASEROW_PUBLIC_URL": "${url}"},
			Volumes: []model.Volume{vol("data", "/baserow/data")},
		}},
	},
	{
		ID: "privatebin", Title: "PrivateBin", Category: "Security", Website: "https://privatebin.info",
		Description: "Minimalist pastebin; pastes are encrypted in the browser and the server never sees them.",
		Services: []Service{{
			Name: "privatebin", Image: "docker.io/privatebin/nginx-fpm-alpine:latest", Type: "web", Port: 8080, Size: "nano",
			Volumes: []model.Volume{vol("data", "/srv/data")},
		}},
	},
	{
		ID: "firefly-iii", Title: "Firefly III", Category: "Finance", Website: "https://www.firefly-iii.org",
		Description: "Personal finance manager: accounts, budgets and reports.",
		Databases:   []Database{{Name: "firefly", Engine: "postgres"}},
		Services: []Service{{
			Name: "firefly", Image: "docker.io/fireflyiii/core:latest", Type: "web", Port: 8080, Size: "small",
			Env: map[string]string{
				"APP_KEY": "${random:32}", "APP_URL": "${url}", "TRUSTED_PROXIES": "**", "DB_CONNECTION": "pgsql",
				"DB_HOST": "${db.firefly.host}", "DB_PORT": "${db.firefly.port}", "DB_DATABASE": "${db.firefly.name}",
				"DB_USERNAME": "${db.firefly.user}", "DB_PASSWORD": "${db.firefly.password}", "TZ": "UTC",
			},
			Volumes: []model.Volume{vol("upload", "/var/www/html/storage/upload")},
		}},
	},
	{
		ID: "bookstack", Title: "BookStack", Category: "Notes", Website: "https://www.bookstackapp.com",
		Description: "Wiki organised as shelves, books and pages. Sign in with admin@admin.com / password and change it.",
		Databases:   []Database{{Name: "bookstack", Engine: "mariadb"}},
		Services: []Service{{
			Name: "bookstack", Image: "lscr.io/linuxserver/bookstack:latest", Type: "web", Port: 80, Size: "small",
			Env: map[string]string{
				"APP_URL": "${url}", "APP_KEY": "${random:32}", "DB_HOST": "${db.bookstack.host}", "DB_PORT": "${db.bookstack.port}",
				"DB_DATABASE": "${db.bookstack.name}", "DB_USERNAME": "${db.bookstack.user}", "DB_PASSWORD": "${db.bookstack.password}",
				"PUID": "1000", "PGID": "1000", "TZ": "Etc/UTC",
			},
			Volumes: []model.Volume{vol("config", "/config")},
		}},
	},

	// Monitoring and development tools
	{
		ID: "beszel", Title: "Beszel", Category: "Monitoring", Website: "https://beszel.dev",
		Description: "Lightweight server monitoring hub; install its agent on the servers to watch.",
		Services: []Service{{
			Name: "beszel", Image: "docker.io/henrygd/beszel:latest", Type: "web", Port: 8090, Size: "micro",
			Env:     map[string]string{"APP_URL": "${url}"},
			Volumes: []model.Volume{vol("data", "/beszel_data")},
		}},
	},
	{
		ID: "mailpit", Title: "Mailpit", Category: "Development", Website: "https://mailpit.axllent.org",
		Description: "Catches the emails your apps send, with a web UI to read them. Apps send to SMTP mailpit:1025.",
		Services: []Service{{
			Name: "mailpit", Image: "docker.io/axllent/mailpit:latest", Type: "web", Port: 8025, Size: "nano",
			Env:     map[string]string{"MP_DATABASE": "/data/mailpit.db", "MP_SMTP_AUTH_ACCEPT_ANY": "true", "MP_SMTP_AUTH_ALLOW_INSECURE": "true"},
			Inputs:  []EnvInput{{Key: "MP_UI_AUTH", Required: true, Placeholder: "admin:password", Hint: "user:password to open the web UI"}},
			Volumes: []model.Volume{vol("data", "/data")},
		}},
	},
}
