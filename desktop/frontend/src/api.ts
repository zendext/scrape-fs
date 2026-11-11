import { Call } from "@wailsio/runtime";

export interface Failure {
  code: string;
  detail?: string;
}
export class RequestError extends Error {
  constructor(public failure: Failure) {
    super(failure.code);
  }
}
export async function request<T>(
  command: string,
  args: Record<string, unknown> = {},
): Promise<T> {
  const response = (await Call.ByName(
    "github.com/zendext/scrape-fs/desktop/internal/gui.Service.Execute",
    { command, ...args },
  )) as { data: T; error?: Failure };
  if (response.error) throw new RequestError(response.error);
  return response.data;
}
export interface Entry {
  id: number;
  name: string;
  path: string;
  kind: "directory" | "mapping" | "file";
  size: number;
  modified: number;
  source?: string;
  can_rebind: boolean;
  available: boolean;
}
export interface Listing {
  library: string;
  path: string;
  id: number;
  entries: Entry[];
}
export interface Library {
  name: string;
  status: string;
  profile_name: string;
  profile_version: number;
  root_node_id: number;
}
export interface Component {
  state: string;
  path?: string;
}
export interface Status {
  can_admin: boolean;
  state: string;
  daemon: { version: string; ready: boolean; configuration_digest?: string };
  mount: Component;
  socket: Component;
  database: Component;
  object_store: Component;
  namespace: Component & {
    projections: number;
    overlay_files: number;
    nodes: number;
  };
}
export interface State {
  system: {
    installed: boolean;
    active: string;
    substate: string;
    result: string;
    available: boolean;
  };
  daemon?: Status;
  error?: Failure;
  system_managed: boolean;
}
export interface Removal {
  library: string;
  path: string;
  directories: number;
  mappings: number;
  files: number;
  unavailable: number;
  digest: string;
}
export interface Document {
  text: string;
  digest: string;
  effective_digest: string;
  fields: Record<string, string>;
  validation_error?: string;
}
export interface ConfigPreview {
  document: Document;
  previous_digest: string;
  changes: { key: string; before: string; after: string }[];
  storage_changed: boolean;
}
export interface Doctor {
  severity: string;
  summary: { healthy: number; warning: number; blocking: number };
  findings: {
    code: string;
    severity: string;
    library?: string;
    target?: string;
    component: string;
  }[];
}
