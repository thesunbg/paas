// Vietnamese dictionaries, one file per area of the dashboard so they can be
// edited without conflicts. Keys are the English strings passed to t().
import common from './common.json';
import auth from './auth.json';
import projects from './projects.json';
import service from './service.json';
import databases from './databases.json';
import nodes from './nodes.json';
import settings from './settings.json';

export const vi: Record<string, string> = { ...common, ...auth, ...projects, ...service, ...databases, ...nodes, ...settings };
