package templates

import "github.com/thesunbg/paas/internal/model"

// More one-click apps, picked from the most popular entries of the selfh.st
// directory that run as a single container (plus managed databases). Images
// are the projects' official ones; data lives in volumes, so it survives
// deploys and is covered by volume backups.
func init() { All = append(All, apps...) }

func vol(name, path string) model.Volume { return model.Volume{Name: name, Path: path} }

func secretInput(key, hint string) EnvInput {
	return EnvInput{Key: key, Required: true, Hint: hint}
}

var apps = []Template{
	// AI
	{
		ID: "open-webui", Title: "Open WebUI", Category: "AI", Website: "https://openwebui.com",
		Description: "ChatGPT-style interface for OpenAI-compatible APIs and Ollama. The first account created becomes the admin.",
		Services: []Service{{
			Name: "open-webui", Image: "ghcr.io/open-webui/open-webui:main", Type: "web", Port: 8080, Size: "medium",
			Env:     map[string]string{"WEBUI_SECRET_KEY": "${random}", "WEBUI_URL": "${url}"},
			Inputs:  []EnvInput{{Key: "OPENAI_API_KEY", Hint: "Optional: an OpenAI (or compatible) API key; can also be added later in the admin settings"}},
			Volumes: []model.Volume{vol("data", "/app/backend/data")},
			Health:  "/health",
		}},
	},
	{
		ID: "flowise", Title: "Flowise", Category: "AI", Website: "https://flowiseai.com",
		Description: "Build LLM apps and AI agents with a drag-and-drop editor.",
		Services: []Service{{
			Name: "flowise", Image: "docker.io/flowiseai/flowise:latest", Type: "web", Port: 3000, Size: "medium",
			Env:     map[string]string{"PORT": "3000", "DATABASE_PATH": "/root/.flowise", "SECRETKEY_PATH": "/root/.flowise", "LOG_PATH": "/root/.flowise/logs", "BLOB_STORAGE_PATH": "/root/.flowise/storage"},
			Volumes: []model.Volume{vol("data", "/root/.flowise")},
		}},
	},

	// Productivity and notes
	{
		ID: "excalidraw", Title: "Excalidraw", Category: "Productivity", Website: "https://excalidraw.com",
		Description: "Virtual whiteboard for hand-drawn style diagrams. Drawings stay in the browser.",
		Services:    []Service{{Name: "excalidraw", Image: "docker.io/excalidraw/excalidraw:latest", Type: "web", Port: 80, Size: "nano"}},
	},
	{
		ID: "it-tools", Title: "IT Tools", Category: "Productivity", Website: "https://it-tools.tech",
		Description: "Handy online tools for developers: encoders, converters, generators and more.",
		Services:    []Service{{Name: "it-tools", Image: "docker.io/corentinth/it-tools:latest", Type: "web", Port: 80, Size: "nano"}},
	},
	{
		ID: "stirling-pdf", Title: "Stirling PDF", Category: "Productivity", Website: "https://stirlingpdf.com",
		Description: "Merge, split, convert, sign and OCR PDF files, all on your own server.",
		Services: []Service{{
			Name: "stirling-pdf", Image: "docker.io/stirlingtools/stirling-pdf:latest", Type: "web", Port: 8080, Size: "medium", HealthTimeout: 420,
			Volumes: []model.Volume{vol("configs", "/configs")},
		}},
	},
	{
		ID: "memos", Title: "Memos", Category: "Notes", Website: "https://usememos.com",
		Description: "Lightweight note-taking for quick capture. The first account created becomes the admin.",
		Services: []Service{{
			Name: "memos", Image: "docker.io/neosmemo/memos:stable", Type: "web", Port: 5230, Size: "micro",
			Volumes: []model.Volume{vol("data", "/var/opt/memos")},
		}},
	},
	{
		ID: "trilium", Title: "Trilium Notes", Category: "Notes", Website: "https://triliumnotes.org",
		Description: "Hierarchical note-taking and personal knowledge base.",
		Services: []Service{{
			Name: "trilium", Image: "docker.io/triliumnext/trilium:latest", Type: "web", Port: 8080, Size: "small",
			Volumes: []model.Volume{vol("data", "/home/node/trilium-data")},
		}},
	},
	{
		ID: "wikijs", Title: "Wiki.js", Category: "Notes", Website: "https://js.wiki",
		Description: "Powerful wiki with a visual editor, backed by PostgreSQL.",
		Databases:   []Database{{Name: "wiki", Engine: "postgres"}},
		Services: []Service{{
			Name: "wiki", Image: "ghcr.io/requarks/wiki:2", Type: "web", Port: 3000, Size: "small",
			Env: map[string]string{
				"DB_TYPE": "postgres", "DB_HOST": "${db.wiki.host}", "DB_PORT": "${db.wiki.port}",
				"DB_USER": "${db.wiki.user}", "DB_PASS": "${db.wiki.password}", "DB_NAME": "${db.wiki.name}",
			},
			Health: "/healthz",
		}},
	},
	{
		ID: "linkding", Title: "linkding", Category: "Productivity", Website: "https://linkding.link",
		Description: "Minimal, fast bookmark manager.",
		Services: []Service{{
			Name: "linkding", Image: "docker.io/sissbruecker/linkding:latest", Type: "web", Port: 9090, Size: "micro",
			Env:     map[string]string{"LD_SUPERUSER_NAME": "admin"},
			Inputs:  []EnvInput{secretInput("LD_SUPERUSER_PASSWORD", "Password of the admin account")},
			Volumes: []model.Volume{vol("data", "/etc/linkding/data")},
		}},
	},
	{
		ID: "mealie", Title: "Mealie", Category: "Productivity", Website: "https://mealie.io",
		Description: "Recipe manager and meal planner. Sign in with changeme@example.com / MyPassword and change it.",
		Services: []Service{{
			Name: "mealie", Image: "ghcr.io/mealie-recipes/mealie:latest", Type: "web", Port: 9000, Size: "small",
			Env:     map[string]string{"BASE_URL": "${url}", "ALLOW_SIGNUP": "false"},
			Volumes: []model.Volume{vol("data", "/app/data")},
		}},
	},
	{
		ID: "actual", Title: "Actual Budget", Category: "Finance", Website: "https://actualbudget.org",
		Description: "Privacy-focused personal finance and budgeting.",
		Services: []Service{{
			Name: "actual", Image: "docker.io/actualbudget/actual-server:latest", Type: "web", Port: 5006, Size: "micro",
			Volumes: []model.Volume{vol("data", "/data")},
		}},
	},
	{
		ID: "paperless-ngx", Title: "Paperless-ngx", Category: "Documents", Website: "https://docs.paperless-ngx.com",
		Description: "Scan, index and archive your documents with OCR and full-text search.",
		Databases:   []Database{{Name: "paperless", Engine: "postgres"}, {Name: "redis", Engine: "redis", Mode: "dedicated"}},
		Services: []Service{{
			Name: "paperless", Image: "ghcr.io/paperless-ngx/paperless-ngx:latest", Type: "web", Port: 8000, Size: "medium",
			Env: map[string]string{
				"PAPERLESS_REDIS": "${db.redis.url}", "PAPERLESS_DBHOST": "${db.paperless.host}", "PAPERLESS_DBPORT": "${db.paperless.port}",
				"PAPERLESS_DBNAME": "${db.paperless.name}", "PAPERLESS_DBUSER": "${db.paperless.user}", "PAPERLESS_DBPASS": "${db.paperless.password}",
				"PAPERLESS_URL": "${url}", "PAPERLESS_SECRET_KEY": "${random}", "PAPERLESS_ADMIN_USER": "admin",
			},
			Inputs: []EnvInput{secretInput("PAPERLESS_ADMIN_PASSWORD", "Password of the admin account")},
			Volumes: []model.Volume{
				vol("data", "/usr/src/paperless/data"), vol("media", "/usr/src/paperless/media"), vol("consume", "/usr/src/paperless/consume"),
			},
		}},
	},
	{
		ID: "docuseal", Title: "DocuSeal", Category: "Documents", Website: "https://www.docuseal.com",
		Description: "Create, fill and sign digital documents (DocuSign alternative).",
		Databases:   []Database{{Name: "docuseal", Engine: "postgres"}},
		Services: []Service{{
			Name: "docuseal", Image: "docker.io/docuseal/docuseal:latest", Type: "web", Port: 3000, Size: "medium",
			Env:     map[string]string{"DATABASE_URL": "${db.docuseal.url}", "SECRET_KEY_BASE": "${random}${random}"},
			Volumes: []model.Volume{vol("data", "/data")},
		}},
	},

	// Development and data
	{
		ID: "code-server", Title: "code-server", Category: "Development", Website: "https://coder.com/docs/code-server",
		Description: "VS Code in the browser, with a persistent home directory.",
		Services: []Service{{
			Name: "code-server", Image: "lscr.io/linuxserver/code-server:latest", Type: "web", Port: 8443, Size: "medium",
			Env:     map[string]string{"PUID": "1000", "PGID": "1000", "TZ": "Etc/UTC"},
			Inputs:  []EnvInput{secretInput("PASSWORD", "Password to open the editor")},
			Volumes: []model.Volume{vol("config", "/config")},
		}},
	},
	{
		ID: "nocodb", Title: "NocoDB", Category: "Databases", Website: "https://nocodb.com",
		Description: "Airtable alternative: spreadsheets on top of PostgreSQL.",
		Databases:   []Database{{Name: "nocodb", Engine: "postgres"}},
		Services: []Service{{
			Name: "nocodb", Image: "docker.io/nocodb/nocodb:latest", Type: "web", Port: 8080, Size: "small",
			Env: map[string]string{
				"NC_DB":              "pg://${db.nocodb.host}:${db.nocodb.port}?u=${db.nocodb.user}&p=${db.nocodb.password}&d=${db.nocodb.name}",
				"NC_AUTH_JWT_SECRET": "${random}", "NC_PUBLIC_URL": "${url}",
			},
			Volumes: []model.Volume{vol("data", "/usr/app/data")},
		}},
	},
	{
		ID: "directus", Title: "Directus", Category: "CMS", Website: "https://directus.io",
		Description: "Headless CMS and instant REST/GraphQL API over PostgreSQL.",
		Databases:   []Database{{Name: "directus", Engine: "postgres"}},
		Services: []Service{{
			Name: "directus", Image: "docker.io/directus/directus:11", Type: "web", Port: 8055, Size: "medium",
			Env: map[string]string{
				"DB_CLIENT": "pg", "DB_HOST": "${db.directus.host}", "DB_PORT": "${db.directus.port}", "DB_DATABASE": "${db.directus.name}",
				"DB_USER": "${db.directus.user}", "DB_PASSWORD": "${db.directus.password}", "SECRET": "${random}", "PUBLIC_URL": "${url}",
			},
			Inputs: []EnvInput{
				{Key: "ADMIN_EMAIL", Required: true, Placeholder: "you@example.com", Hint: "Email of the first admin"},
				secretInput("ADMIN_PASSWORD", "Password of the first admin"),
			},
			Volumes: []model.Volume{vol("uploads", "/directus/uploads"), vol("extensions", "/directus/extensions")},
			Health:  "/server/health",
		}},
	},
	{
		ID: "meilisearch", Title: "Meilisearch", Category: "Development", Website: "https://www.meilisearch.com",
		Description: "Lightning-fast search API. The master key is in the service's Environment.",
		Services: []Service{{
			Name: "meilisearch", Image: "docker.io/getmeili/meilisearch:latest", Type: "web", Port: 7700, Size: "small",
			Env:     map[string]string{"MEILI_MASTER_KEY": "${random}", "MEILI_ENV": "production", "MEILI_NO_ANALYTICS": "true"},
			Volumes: []model.Volume{vol("data", "/meili_data")},
			Health:  "/health",
		}},
	},
	{
		ID: "keycloak", Title: "Keycloak", Category: "Security", Website: "https://www.keycloak.org",
		Description: "Identity and access management: single sign-on, OIDC and SAML.",
		Databases:   []Database{{Name: "keycloak", Engine: "postgres"}},
		Services: []Service{{
			Name: "keycloak", Image: "quay.io/keycloak/keycloak:26.4", Type: "web", Port: 8080, Size: "medium", HealthTimeout: 600,
			Command: []string{"/opt/keycloak/bin/kc.sh", "start"},
			Env: map[string]string{
				"KC_DB": "postgres", "KC_DB_URL": "jdbc:postgresql://${db.keycloak.host}:${db.keycloak.port}/${db.keycloak.name}",
				"KC_DB_USERNAME": "${db.keycloak.user}", "KC_DB_PASSWORD": "${db.keycloak.password}",
				"KC_HOSTNAME": "${url}", "KC_HTTP_ENABLED": "true", "KC_PROXY_HEADERS": "xforwarded", "KC_BOOTSTRAP_ADMIN_USERNAME": "admin",
			},
			Inputs: []EnvInput{secretInput("KC_BOOTSTRAP_ADMIN_PASSWORD", "Password of the temporary admin account")},
		}},
	},

	// Security
	{
		ID: "vaultwarden", Title: "Vaultwarden", Category: "Security", Website: "https://github.com/dani-garcia/vaultwarden",
		Description: "Bitwarden-compatible password manager server; works with the official Bitwarden apps.",
		Services: []Service{{
			Name: "vaultwarden", Image: "docker.io/vaultwarden/server:latest", Type: "web", Port: 80, Size: "micro",
			Env:     map[string]string{"DOMAIN": "${url}", "SIGNUPS_ALLOWED": "true"},
			Volumes: []model.Volume{vol("data", "/data")},
			Health:  "/alive",
		}},
	},

	// Monitoring and notifications
	{
		ID: "grafana", Title: "Grafana", Category: "Monitoring", Website: "https://grafana.com",
		Description: "Dashboards and alerting for metrics, logs and SQL databases.",
		Databases:   []Database{{Name: "grafana", Engine: "postgres"}},
		Services: []Service{{
			Name: "grafana", Image: "docker.io/grafana/grafana-oss:latest", Type: "web", Port: 3000, Size: "small",
			Env: map[string]string{
				"GF_SERVER_ROOT_URL": "${url}", "GF_DATABASE_TYPE": "postgres", "GF_DATABASE_HOST": "${db.grafana.host}:${db.grafana.port}",
				"GF_DATABASE_NAME": "${db.grafana.name}", "GF_DATABASE_USER": "${db.grafana.user}", "GF_DATABASE_PASSWORD": "${db.grafana.password}",
				"GF_DATABASE_SSL_MODE": "disable", "GF_SECURITY_ADMIN_USER": "admin",
			},
			Inputs:  []EnvInput{secretInput("GF_SECURITY_ADMIN_PASSWORD", "Password of the admin account")},
			Volumes: []model.Volume{vol("data", "/var/lib/grafana")},
			Health:  "/api/health",
		}},
	},
	{
		ID: "changedetection", Title: "changedetection.io", Category: "Monitoring", Website: "https://changedetection.io",
		Description: "Watch web pages for changes and get alerts.",
		Services: []Service{{
			Name: "changedetection", Image: "ghcr.io/dgtlmoon/changedetection.io:latest", Type: "web", Port: 5000, Size: "small",
			Env:     map[string]string{"BASE_URL": "${url}"},
			Volumes: []model.Volume{vol("data", "/datastore")},
		}},
	},
	{
		ID: "ntfy", Title: "ntfy", Category: "Notifications", Website: "https://ntfy.sh",
		Description: "Send push notifications to your phone or desktop with a simple HTTP request.",
		Services: []Service{{
			Name: "ntfy", Image: "docker.io/binwiederhier/ntfy:latest", Type: "web", Port: 80, Size: "nano",
			Command: []string{"ntfy", "serve"},
			Env: map[string]string{
				"NTFY_BASE_URL": "${url}", "NTFY_LISTEN_HTTP": ":80", "NTFY_BEHIND_PROXY": "true",
				"NTFY_CACHE_FILE": "/var/cache/ntfy/cache.db", "NTFY_ATTACHMENT_CACHE_DIR": "/var/cache/ntfy/attachments",
			},
			Volumes: []model.Volume{vol("cache", "/var/cache/ntfy")},
			Health:  "/v1/health",
		}},
	},
	{
		ID: "gotify", Title: "Gotify", Category: "Notifications", Website: "https://gotify.net",
		Description: "Simple server for sending and receiving push messages.",
		Services: []Service{{
			Name: "gotify", Image: "docker.io/gotify/server:latest", Type: "web", Port: 80, Size: "nano",
			Env:     map[string]string{"GOTIFY_DEFAULTUSER_NAME": "admin"},
			Inputs:  []EnvInput{secretInput("GOTIFY_DEFAULTUSER_PASS", "Password of the admin account")},
			Volumes: []model.Volume{vol("data", "/app/data")},
			Health:  "/health",
		}},
	},
	{
		ID: "homepage", Title: "Homepage", Category: "Dashboards", Website: "https://gethomepage.dev",
		Description: "Customizable start page for your services; edit the YAML files in its config volume.",
		Services: []Service{{
			Name: "homepage", Image: "ghcr.io/gethomepage/homepage:latest", Type: "web", Port: 3000, Size: "micro",
			Env:     map[string]string{"HOMEPAGE_ALLOWED_HOSTS": "${host}"},
			Volumes: []model.Volume{vol("config", "/app/config")},
		}},
	},

	// Communication and content
	{
		ID: "ghost", Title: "Ghost", Category: "CMS", Website: "https://ghost.org",
		Description: "Blog and newsletter platform, with MySQL. Open /ghost to create the owner account.",
		Databases:   []Database{{Name: "ghost", Engine: "mysql"}},
		Services: []Service{{
			Name: "ghost", Image: "docker.io/library/ghost:6-alpine", Type: "web", Port: 2368, Size: "small",
			Env: map[string]string{
				"url": "${url}", "database__client": "mysql", "database__connection__host": "${db.ghost.host}",
				"database__connection__port": "${db.ghost.port}", "database__connection__user": "${db.ghost.user}",
				"database__connection__password": "${db.ghost.password}", "database__connection__database": "${db.ghost.name}",
			},
			Volumes: []model.Volume{vol("content", "/var/lib/ghost/content")},
		}},
	},
	{
		ID: "mattermost", Title: "Mattermost", Category: "Communication", Website: "https://mattermost.com",
		Description: "Team chat and collaboration (Slack alternative). The first account created becomes the admin.",
		Databases:   []Database{{Name: "mattermost", Engine: "postgres"}},
		Services: []Service{{
			Name: "mattermost", Image: "docker.io/mattermost/mattermost-team-edition:latest", Type: "web", Port: 8065, Size: "medium", HealthTimeout: 420,
			Env: map[string]string{
				"MM_SQLSETTINGS_DRIVERNAME": "postgres", "MM_SQLSETTINGS_DATASOURCE": "${db.mattermost.url}&connect_timeout=10",
				"MM_SERVICESETTINGS_SITEURL": "${url}",
			},
			Volumes: []model.Volume{
				vol("data", "/mattermost/data"), vol("plugins", "/mattermost/plugins"), vol("client-plugins", "/mattermost/client/plugins"),
			},
			Health: "/api/v4/system/ping",
		}},
	},
	{
		ID: "listmonk", Title: "listmonk", Category: "Communication", Website: "https://listmonk.app",
		Description: "Newsletter and mailing list manager backed by PostgreSQL.",
		Databases:   []Database{{Name: "listmonk", Engine: "postgres"}},
		Services: []Service{{
			Name: "listmonk", Image: "docker.io/listmonk/listmonk:latest", Type: "web", Port: 9000, Size: "small",
			Command: []string{"./listmonk --install --idempotent --yes --config '' && ./listmonk --upgrade --yes --config '' && exec ./listmonk --config ''"},
			Env: map[string]string{
				"LISTMONK_app__address": "0.0.0.0:9000", "LISTMONK_db__host": "${db.listmonk.host}", "LISTMONK_db__port": "${db.listmonk.port}",
				"LISTMONK_db__user": "${db.listmonk.user}", "LISTMONK_db__password": "${db.listmonk.password}",
				"LISTMONK_db__database": "${db.listmonk.name}", "LISTMONK_db__ssl_mode": "disable", "LISTMONK_ADMIN_USER": "admin",
			},
			Inputs:  []EnvInput{secretInput("LISTMONK_ADMIN_PASSWORD", "Password of the admin account")},
			Volumes: []model.Volume{vol("uploads", "/listmonk/uploads")},
		}},
	},
	{
		ID: "searxng", Title: "SearXNG", Category: "Search", Website: "https://docs.searxng.org",
		Description: "Privacy-respecting metasearch engine.",
		Services: []Service{{
			Name: "searxng", Image: "docker.io/searxng/searxng:latest", Type: "web", Port: 8080, Size: "small",
			Env:     map[string]string{"SEARXNG_BASE_URL": "${url}/", "SEARXNG_SECRET": "${random}"},
			Volumes: []model.Volume{vol("config", "/etc/searxng")},
		}},
	},
	{
		ID: "freshrss", Title: "FreshRSS", Category: "Feeds", Website: "https://freshrss.org",
		Description: "RSS feed aggregator; the setup wizard runs on first visit.",
		Services: []Service{{
			Name: "freshrss", Image: "docker.io/freshrss/freshrss:latest", Type: "web", Port: 80, Size: "micro",
			Env:     map[string]string{"CRON_MIN": "*/20", "TZ": "Etc/UTC"},
			Volumes: []model.Volume{vol("data", "/var/www/FreshRSS/data"), vol("extensions", "/var/www/FreshRSS/extensions")},
		}},
	},
	{
		ID: "miniflux", Title: "Miniflux", Category: "Feeds", Website: "https://miniflux.app",
		Description: "Minimalist and fast RSS reader backed by PostgreSQL.",
		Databases:   []Database{{Name: "miniflux", Engine: "postgres"}},
		Services: []Service{{
			Name: "miniflux", Image: "docker.io/miniflux/miniflux:latest", Type: "web", Port: 8080, Size: "micro",
			Env: map[string]string{
				"DATABASE_URL": "${db.miniflux.url}", "RUN_MIGRATIONS": "1", "CREATE_ADMIN": "1", "ADMIN_USERNAME": "admin",
				"BASE_URL": "${url}", "LISTEN_ADDR": "0.0.0.0:8080",
			},
			Inputs: []EnvInput{secretInput("ADMIN_PASSWORD", "Password of the admin account (6 characters or more)")},
			Health: "/healthcheck",
		}},
	},

	// Media and files
	{
		ID: "jellyfin", Title: "Jellyfin", Category: "Media", Website: "https://jellyfin.org",
		Description: "Media server for movies, shows and music. Upload media into its media volume.",
		Services: []Service{{
			Name: "jellyfin", Image: "docker.io/jellyfin/jellyfin:latest", Type: "web", Port: 8096, Size: "medium", HealthTimeout: 300,
			Env:     map[string]string{"JELLYFIN_PublishedServerUrl": "${url}"},
			Volumes: []model.Volume{vol("config", "/config"), vol("cache", "/cache"), vol("media", "/media")},
			Health:  "/health",
		}},
	},
	{
		ID: "navidrome", Title: "Navidrome", Category: "Media", Website: "https://www.navidrome.org",
		Description: "Music streaming server compatible with Subsonic apps.",
		Services: []Service{{
			Name: "navidrome", Image: "docker.io/deluan/navidrome:latest", Type: "web", Port: 4533, Size: "micro",
			Volumes: []model.Volume{vol("data", "/data"), vol("music", "/music")},
			Health:  "/ping",
		}},
	},
	{
		ID: "nextcloud", Title: "Nextcloud", Category: "Files", Website: "https://nextcloud.com",
		Description: "File sync and share, calendar, contacts and office apps, with PostgreSQL.",
		Databases:   []Database{{Name: "nextcloud", Engine: "postgres"}},
		Services: []Service{{
			Name: "nextcloud", Image: "docker.io/library/nextcloud:apache", Type: "web", Port: 80, Size: "medium", HealthTimeout: 420,
			Env: map[string]string{
				"POSTGRES_HOST": "${db.nextcloud.host}:${db.nextcloud.port}", "POSTGRES_DB": "${db.nextcloud.name}",
				"POSTGRES_USER": "${db.nextcloud.user}", "POSTGRES_PASSWORD": "${db.nextcloud.password}",
				"NEXTCLOUD_TRUSTED_DOMAINS": "${host}", "OVERWRITEPROTOCOL": "https", "TRUSTED_PROXIES": "10.0.0.0/8",
				"NEXTCLOUD_ADMIN_USER": "admin",
			},
			Inputs:  []EnvInput{secretInput("NEXTCLOUD_ADMIN_PASSWORD", "Password of the admin account")},
			Volumes: []model.Volume{vol("html", "/var/www/html")},
		}},
	},
}
