-- net_index 1022 is the BuildKit network (network.BuildNetIndex); 1023 is infrastructure.
ALTER TABLE projects ADD CONSTRAINT projects_net_index_tenant CHECK (net_index < 1022);
