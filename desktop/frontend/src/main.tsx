import {
  For,
  Show,
  createEffect,
  createMemo,
  createSignal,
  onCleanup,
  onMount,
} from "solid-js";
import { render } from "solid-js/web";
import {
  request,
  RequestError,
  type Listing,
  type Library,
  type Entry,
  type State,
  type Failure,
  type Removal,
  type Document,
  type ConfigPreview,
  type Doctor,
} from "./api";
import { translate, findingText, type Language } from "./i18n";
import "./style.css";

type Dialog = {
  kind: string;
  library?: string;
  path?: string;
  id?: number;
  entry?: Entry;
};
const join = (parent: string, name: string) =>
  parent ? `${parent}/${name}` : name;
const parent = (path: string) =>
  path.includes("/") ? path.slice(0, path.lastIndexOf("/")) : "";
const key = (library: string, path: string) => `${library}\0${path}`;
const configKeys = [
  "storage.database_path",
  "storage.overlay_store_path",
  "fuse.mountpoint",
  "fuse.entry_timeout",
  "fuse.attr_timeout",
  "fuse.negative_timeout",
  "control.socket_path",
  "control.socket_mode",
  "control.administrator_uid",
  "gc.orphan_grace_period",
];

function Icon(props: { kind: string }) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="1.6"
      aria-hidden="true"
    >
      <Show
        when={props.kind === "directory"}
        fallback={
          <>
            <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
            <path d="M14 2v6h6" />
            <Show when={props.kind === "mapping"}>
              <path d="m9 14 3-3 3 3m-3-3v7" />
            </Show>
          </>
        }
      >
        <path d="M3 7V5a2 2 0 0 1 2-2h5l2 3h7a2 2 0 0 1 2 2v11a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" />
      </Show>
    </svg>
  );
}

function App() {
  const [language, setLanguage] = createSignal<Language>("zh");
  const t = (name: string) => translate(language(), name);
  const [page, setPage] = createSignal("libraries");
  const [state, setState] = createSignal<State>();
  const [libraries, setLibraries] = createSignal<Library[]>([]);
  const [library, setLibrary] = createSignal("");
  const [path, setPath] = createSignal("");
  const [address, setAddress] = createSignal("");
  const [listing, setListing] = createSignal<Listing>();
  const [cache, setCache] = createSignal<Record<string, Listing>>({});
  const [expanded, setExpanded] = createSignal<Set<string>>(new Set());
  const [treeErrors, setTreeErrors] = createSignal<Record<string, Failure>>({});
  const pendingTrees = new Set<string>();
  const [selection, setSelection] = createSignal<Entry>();
  const [details, setDetails] = createSignal(false);
  const [width, setWidth] = createSignal(260);
  const [busy, setBusy] = createSignal(false);
  const [loading, setLoading] = createSignal(false);
  const [error, setError] = createSignal<Failure>();
  const [notice, setNotice] = createSignal("");
  const [dialog, setDialog] = createSignal<Dialog>();
  const [formName, setFormName] = createSignal("");
  const [profile, setProfile] = createSignal("jellyfin@1");
  const [target, setTarget] = createSignal("");
  const [source, setSource] = createSignal("");
  const [removal, setRemoval] = createSignal<Removal>();
  const [rebind, setRebind] = createSignal<{
    confirmation_digest: string;
    proposed_source: { size: number; path: string };
  }>();
  const [dialogError, setDialogError] = createSignal<Failure>();
  const [document, setDocument] = createSignal<Document>();
  const [fields, setFields] = createSignal<Record<string, string>>({});
  const [raw, setRaw] = createSignal(false);
  const [rawText, setRawText] = createSignal("");
  const [configPreview, setConfigPreview] = createSignal<ConfigPreview>();
  const [configError, setConfigError] = createSignal<Failure>();
  const [doctor, setDoctor] = createSignal<Doctor>();
  const [checkedAt, setCheckedAt] = createSignal<number>();
  let addressInput!: HTMLInputElement;
  let viewport!: HTMLDivElement;
  let modal!: HTMLDialogElement;
  let navigation = 0;
  let languageChosen = false;
  let preferenceSave = Promise.resolve();
  const ready = () => !!state()?.daemon?.daemon.ready;
  const admin = () => ready() && !!state()?.daemon?.can_admin;
  const currentLibrary = () =>
    libraries().find((item) => item.name === library());
  const selected = () => selection();
  const dirty = createMemo(
    () =>
      !!document() &&
      (raw()
        ? rawText() !== document()!.text
        : configKeys.some(
            (name) => fields()[name] !== document()!.fields?.[name],
          )),
  );
  const failureOf = (err: unknown): Failure =>
    err instanceof RequestError ? err.failure : { code: "OperationFailed" };
  function errorText(value?: Failure): string {
    if (!value) return "";
    const text = t(value.code);
    return (
      (text === value.code ? t("OperationFailed") : text) +
      (value.detail ? ` ${value.detail}` : "")
    );
  }
  const size = (bytes: number) => {
    if (bytes < 1024) return `${bytes} B`;
    const unit = Math.min(4, Math.floor(Math.log(bytes) / Math.log(1024)));
    return `${(bytes / 1024 ** unit).toFixed(unit > 1 ? 1 : 0)} ${["B", "KiB", "MiB", "GiB", "TiB"][unit]}`;
  };
  const date = (time: number) =>
    new Date(time).toLocaleString(language() === "zh" ? "zh-CN" : "en-GB", {
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
    });

  async function updateState() {
    try {
      const next = await request<State>("state");
      setState(next);
      return next;
    } catch (err) {
      setError(failureOf(err));
    }
  }
  async function loadLibraries() {
    const result = await request<{ libraries: Library[] }>("libraries");
    const active = result.libraries.filter((item) => item.status === "ACTIVE");
    setLibraries(active);
    return active;
  }
  async function readTree(name: string, directory: string) {
    const id = key(name, directory);
    if (pendingTrees.has(id)) return;
    pendingTrees.add(id);
    try {
      const result = await request<Listing>("browse", {
        library: name,
        path: directory,
      });
      setCache((value) => ({ ...value, [id]: result }));
    } catch (err) {
      setTreeErrors((value) => ({ ...value, [id]: failureOf(err) }));
    } finally {
      pendingTrees.delete(id);
    }
  }
  async function navigate(name: string, directory: string) {
    const serial = ++navigation;
    setLoading(true);
    setError(undefined);
    setNotice("");
    try {
      const result = await request<Listing>("browse", {
        library: name,
        path: directory,
      });
      if (serial !== navigation) return;
      setLibrary(name);
      setPath(directory);
      setListing(result);
      setAddress(`${name}:/${directory}`);
      setSelection(undefined);
      setDetails(false);
      setCache((value) => ({ ...value, [key(name, directory)]: result }));
      const ancestors = [
        "",
        ...directory
          .split("/")
          .filter(Boolean)
          .map((_, index, all) => all.slice(0, index + 1).join("/")),
      ];
      setExpanded(
        (value) =>
          new Set([...value, ...ancestors.map((item) => key(name, item))]),
      );
      viewport?.scrollTo(0, 0);
    } catch (err) {
      if (serial === navigation) {
        setError(failureOf(err));
        setListing(undefined);
      }
    } finally {
      if (serial === navigation) setLoading(false);
    }
  }
  async function refresh() {
    setError(undefined);
    setTreeErrors({});
    setCache({});
    const next = await updateState();
    if (!next?.daemon?.daemon.ready) {
      setListing(undefined);
      return;
    }
    try {
      const active = await loadLibraries();
      const name = active.some((item) => item.name === library())
        ? library()
        : active[0]?.name;
      if (name) await navigate(name, name === library() ? path() : "");
      else {
        setLibrary("");
        setListing(undefined);
      }
    } catch (err) {
      setError(failureOf(err));
    }
  }
  async function submitAddress(event: SubmitEvent) {
    event.preventDefault();
    const match = /^([^/:]+):\/(.*)$/.exec(address());
    if (
      !match ||
      (match[2] &&
        match[2]
          .split("/")
          .some((part) => !part || part === "." || part === "..")) ||
      !libraries().some((item) => item.name === match[1])
    ) {
      setError({ code: "InvalidPath" });
      return;
    }
    await navigate(match[1], match[2]);
  }
  async function perform(command: string, args: Record<string, unknown> = {}) {
    setError(undefined);
    try {
      return await request(command, args);
    } catch (err) {
      setError(failureOf(err));
    }
  }
  function showDialog(value: Dialog) {
    setDialogError(undefined);
    setRemoval(undefined);
    setRebind(undefined);
    setFormName(value.library ?? "");
    setTarget(value.path ?? "");
    setSource("");
    const item = libraries().find((item) => item.name === value.library);
    setProfile(
      item ? `${item.profile_name}@${item.profile_version}` : "jellyfin@1",
    );
    setDialog(value);
  }
  createEffect(() => {
    if (dialog()) {
      if (!modal.open) modal.show();
    } else if (modal.open) modal.close();
  });
  function context(kind: string, entry?: Entry): Dialog {
    return {
      kind,
      library: library(),
      path: entry?.path ?? path(),
      id: entry?.id ?? listing()?.id,
      entry,
    };
  }
  async function previewRemoval() {
    setBusy(true);
    setDialogError(undefined);
    try {
      setRemoval(
        await request<Removal>("preview_removal", {
          library: dialog()?.library,
          path: dialog()?.path,
        }),
      );
    } catch (err) {
      setDialogError(failureOf(err));
    } finally {
      setBusy(false);
    }
  }
  async function submitDialog(event: SubmitEvent) {
    event.preventDefault();
    const operation = dialog();
    if (!operation) return;
    setBusy(true);
    setDialogError(undefined);
    let nextLibrary = library(),
      nextPath = path();
    try {
      const args = {
        library: operation.library,
        path: operation.path,
        id: operation.id,
      };
      switch (operation.kind) {
        case "newLibrary":
          await request("create_library", {
            name: formName(),
            profile: profile(),
          });
          nextLibrary = formName();
          nextPath = "";
          break;
        case "rename":
          await request("rename_library", { ...args, name: formName() });
          nextLibrary = formName();
          break;
        case "changeProfile":
          await request("profile_library", { ...args, profile: profile() });
          break;
        case "newFolder":
          await request("create_directory", { ...args, name: formName() });
          break;
        case "move":
          await request("move_directory", { ...args, target: target() });
          if (
            path() === operation.path ||
            path().startsWith(`${operation.path}/`)
          )
            nextPath = target() + path().slice(operation.path!.length);
          break;
        case "removeFolder":
        case "removeLibrary":
          if (!removal()) return;
          await request("remove_tree", { ...args, digest: removal()!.digest });
          if (operation.kind === "removeLibrary") {
            nextLibrary = "";
            nextPath = "";
          } else if (
            path() === operation.path ||
            path().startsWith(`${operation.path}/`)
          )
            nextPath = parent(operation.path!);
          break;
        case "addMapping":
          await request("create_mapping", {
            ...args,
            source: source(),
            target: target(),
          });
          break;
        case "moveMapping":
          await request("move_mapping", { ...args, target: target() });
          break;
        case "removeMapping":
          await request("remove_mapping", args);
          break;
        case "rebind": {
          const result = await request<{
            status: string;
            confirmation_digest: string;
            proposed_source: { size: number; path: string };
          }>("rebind_mapping", {
            ...args,
            source: source(),
            digest: rebind()?.confirmation_digest ?? "",
          });
          if (result.status !== "applied") {
            setRebind(result);
            return;
          }
          break;
        }
        case "stop":
        case "restart":
          setState(await request<State>("service", { action: operation.kind }));
          break;
        default:
          return;
      }
      setDialog(undefined);
      setCache({});
      setTreeErrors({});
      setDoctor(undefined);
      setCheckedAt(undefined);
      await updateState();
      if (ready()) {
        const active = await loadLibraries();
        nextLibrary = active.some((item) => item.name === nextLibrary)
          ? nextLibrary
          : (active[0]?.name ?? "");
        if (nextLibrary) await navigate(nextLibrary, nextPath);
        else {
          setLibrary("");
          setListing(undefined);
        }
      } else setListing(undefined);
      setNotice("success");
    } catch (err) {
      const failure = failureOf(err);
      setDialogError(failure);
      if (failure.code === "ConfirmationMismatch") {
        setRemoval(undefined);
        setRebind(undefined);
      }
    } finally {
      setBusy(false);
    }
  }
  async function chooseSource() {
    try {
      const chosen = await request<string>("choose_source");
      if (chosen) {
        setSource(chosen);
        setRebind(undefined);
        if (dialog()?.kind === "addMapping")
          setTarget(join(path(), chosen.split("/").pop()!));
      }
    } catch (err) {
      setDialogError(failureOf(err));
    }
  }
  function setConfig(value: Document) {
    setDocument(value);
    setFields({ ...value.fields });
    setRawText(value.text);
    setRaw(!!value.validation_error);
    setConfigPreview(undefined);
    setConfigError(undefined);
  }
  async function loadConfig(authorize = false) {
    setConfigError(undefined);
    try {
      setConfig(
        await request<Document>(
          authorize ? "authorize_configuration" : "configuration",
        ),
      );
      if (authorize) await refresh();
    } catch (err) {
      setConfigError(failureOf(err));
    }
  }
  async function previewConfig(event: SubmitEvent) {
    event.preventDefault();
    setBusy(true);
    setConfigError(undefined);
    setConfigPreview(undefined);
    try {
      setConfigPreview(
        await request<ConfigPreview>("preview_configuration", {
          digest: document()!.digest,
          raw: raw(),
          text: rawText(),
          fields: fields(),
        }),
      );
    } catch (err) {
      setConfigError(failureOf(err));
    } finally {
      setBusy(false);
    }
  }
  async function saveConfig(restart: boolean) {
    const preview = configPreview();
    if (!preview) return;
    setBusy(true);
    setConfigError(undefined);
    try {
      const result = await request<{
        document: Document;
        state: State;
        restart_error?: Failure;
      }>("save_configuration", {
        text: preview.document.text,
        digest: preview.previous_digest,
        action: restart ? "restart" : "",
      });
      setConfig(result.document);
      setState(result.state);
      setNotice(result.restart_error ? "restartFailed" : "saved");
      if (result.restart_error) setConfigError(result.restart_error);
    } catch (err) {
      setConfigError(failureOf(err));
      setConfigPreview(undefined);
    } finally {
      setBusy(false);
    }
  }
  async function diagnostics() {
    setBusy(true);
    setError(undefined);
    try {
      setDoctor(await request<Doctor>("doctor"));
      setCheckedAt(Date.now());
    } catch (err) {
      setError(failureOf(err));
    } finally {
      setBusy(false);
    }
  }
  async function start() {
    setBusy(true);
    try {
      setState(await request<State>("service", { action: "start" }));
      await refresh();
    } catch (err) {
      setError(failureOf(err));
      await updateState();
    } finally {
      setBusy(false);
    }
  }
  async function changeLanguage(value: Language) {
    languageChosen = true;
    setLanguage(value);
    preferenceSave = preferenceSave.then(async () => {
      try {
        await request("save_preferences", { name: value });
      } catch (err) {
        setError(failureOf(err));
      }
    });
    await preferenceSave;
  }

  function resize(event: PointerEvent) {
    const handle = event.currentTarget as HTMLElement;
    handle.setPointerCapture(event.pointerId);
    const start = event.clientX,
      original = width();
    const move = (e: PointerEvent) =>
      setWidth(Math.max(180, Math.min(460, original + e.clientX - start)));
    const stop = () => {
      handle.removeEventListener("pointermove", move);
      handle.removeEventListener("lostpointercapture", stop);
    };
    handle.addEventListener("pointermove", move);
    handle.addEventListener("lostpointercapture", stop);
  }
  onMount(() => {
    void request<{ language: Language }>("preferences")
      .then((value) => {
        if (!languageChosen) setLanguage(value.language);
      })
      .catch(() => {});
    void refresh();
    void loadConfig();
    const interval = setInterval(() => {
      if (!busy()) void updateState();
    }, 15000);
    const keyboard = (event: KeyboardEvent) => {
      if (
        (event.ctrlKey || event.metaKey) &&
        event.key.toLowerCase() === "l" &&
        page() === "libraries" &&
        !modal.open
      ) {
        event.preventDefault();
        addressInput.focus();
        addressInput.select();
      }
    };
    window.addEventListener("keydown", keyboard);
    onCleanup(() => {
      clearInterval(interval);
      window.removeEventListener("keydown", keyboard);
    });
  });
  createEffect(() => {
    window.document.documentElement.lang = language() === "zh" ? "zh-CN" : "en";
  });

  function TreeItem(props: {
    library: string;
    path: string;
    name: string;
    depth: number;
    id?: number;
  }) {
    const id = () => key(props.library, props.path);
    const isExpanded = () => expanded().has(id());
    const children = () =>
      cache()[id()]?.entries.filter((item) => item.kind === "directory") ?? [];
    createEffect(() => {
      if (isExpanded() && !cache()[id()] && !treeErrors()[id()])
        void readTree(props.library, props.path);
    });
    const toggle = () =>
      setExpanded((value) => {
        const next = new Set(value);
        if (next.has(id())) next.delete(id());
        else next.add(id());
        return next;
      });
    return (
      <li>
        <div
          class="tree-row"
          classList={{
            selected: library() === props.library && path() === props.path,
          }}
          style={{ "padding-left": `${7 + props.depth * 14}px` }}
          onContextMenu={(event) => {
            event.preventDefault();
            if (admin())
              showDialog({
                kind: props.path ? "folderActions" : "editLibrary",
                library: props.library,
                path: props.path,
                id: cache()[id()]?.id ?? props.id,
              });
          }}
        >
          <button
            class="toggle"
            aria-label={`${props.name} ${t("directory")}`}
            aria-expanded={isExpanded()}
            onClick={toggle}
          >
            {isExpanded() ? "⌄" : "›"}
          </button>
          <button
            class="folder"
            title={props.name}
            onClick={() => void navigate(props.library, props.path)}
          >
            <Icon kind="directory" />
            <span>{props.name}</span>
          </button>
        </div>
        <Show when={isExpanded()}>
          <ul>
            <For each={children()}>
              {(child) => (
                <TreeItem
                  library={props.library}
                  path={child.path}
                  name={child.name}
                  id={child.id}
                  depth={props.depth + 1}
                />
              )}
            </For>
          </ul>
          <Show when={treeErrors()[id()]}>
            <p class="tree-error" title={errorText(treeErrors()[id()])}>
              {errorText(treeErrors()[id()])}
            </p>
          </Show>
        </Show>
      </li>
    );
  }

  return (
    <div class="app" aria-busy={busy()}>
      <header class="top">
        <strong>ScrapeFS</strong>
        <nav aria-label="ScrapeFS">
          <For each={["libraries", "status", "config"]}>
            {(item) => (
              <button
                class="tab"
                aria-pressed={page() === item}
                onClick={() => setPage(item)}
              >
                {t(item)}
              </button>
            )}
          </For>
        </nav>
        <span class="connection">
          <i classList={{ online: ready() }} />
          {t(ready() ? "ready" : "unavailable")}
        </span>
        <select
          class="language"
          aria-label={t("language")}
          value={language()}
          onChange={(event) =>
            void changeLanguage(event.currentTarget.value as Language)
          }
        >
          <option value="zh">中文</option>
          <option value="en">English</option>
        </select>
      </header>
      <Show when={error()}>
        <div class="message error" role="alert">
          {errorText(error())}
          <button
            class="dismiss"
            aria-label={t("close")}
            onClick={() => setError(undefined)}
          >
            ×
          </button>
        </div>
      </Show>
      <section class="library-page" hidden={page() !== "libraries"}>
        <div class="toolbar">
          <form class="address-form" onSubmit={submitAddress}>
            <input
              ref={addressInput}
              aria-label={t("address")}
              value={address()}
              onInput={(event) => setAddress(event.currentTarget.value)}
              onKeyDown={(event) => {
                if (event.key === "Escape") {
                  setAddress(`${library()}:/${path()}`);
                  event.currentTarget.blur();
                }
              }}
              spellcheck={false}
              autocomplete="off"
            />
            <button
              type="submit"
              title={t("go")}
              aria-label={t("go")}
              disabled={!ready() || loading()}
            >
              →
            </button>
          </form>
          <button
            onClick={() =>
              void perform("open_folder", { library: library(), path: path() })
            }
            disabled={!listing() || !ready()}
          >
            {t("openFolder")}
          </button>
          <button onClick={() => void refresh()} disabled={loading() || busy()}>
            {t("refresh")}
          </button>
        </div>
        <div
          class="browser"
          style={{ "grid-template-columns": `${width()}px 5px minmax(0,1fr)` }}
        >
          <aside class="tree" aria-label={t("tree")}>
            <div class="tree-heading">
              <span>{t("libraries")}</span>
              <div class="actions">
                <button
                  disabled={!admin() || busy()}
                  onClick={() => {
                    showDialog({ kind: "newLibrary" });
                    setFormName("");
                  }}
                >
                  {t("new")}
                </button>
                <button
                  disabled={!admin() || !library() || busy()}
                  onClick={() => showDialog(context("editLibrary"))}
                >
                  {t("manage")}
                </button>
              </div>
            </div>
            <ul>
              <For each={libraries()}>
                {(item) => (
                  <TreeItem
                    library={item.name}
                    path=""
                    name={item.name}
                    id={item.root_node_id}
                    depth={0}
                  />
                )}
              </For>
            </ul>
          </aside>
          <div
            class="divider"
            role="separator"
            tabIndex={0}
            aria-label={t("resize")}
            aria-orientation="vertical"
            aria-valuemin={180}
            aria-valuemax={460}
            aria-valuenow={width()}
            onPointerDown={resize}
            onKeyDown={(event) => {
              if (["ArrowLeft", "ArrowRight"].includes(event.key)) {
                event.preventDefault();
                setWidth((value) =>
                  Math.max(
                    180,
                    Math.min(
                      460,
                      value + (event.key === "ArrowRight" ? 10 : -10),
                    ),
                  ),
                );
              }
            }}
          />
          <section class="files" aria-label={t("files")}>
            <div class="localbar">
              <span class="current-path" title={`${library()}:/${path()}`}>
                {library() ? `${library()}:/${path()}` : t("selectLibrary")}
              </span>
              <div class="actions">
                <button
                  disabled={!admin() || !listing() || busy()}
                  onClick={() => {
                    showDialog(context("newFolder"));
                    setFormName("");
                  }}
                >
                  {t("newFolder")}
                </button>
                <button
                  disabled={!admin() || !listing() || busy()}
                  onClick={() =>
                    showDialog(
                      context(path() ? "folderActions" : "editLibrary"),
                    )
                  }
                >
                  {t("folderActions")}
                </button>
                <button
                  disabled={!selection() || !ready() || busy()}
                  onClick={() =>
                    selected()?.kind === "directory"
                      ? void navigate(library(), selected()!.path)
                      : void perform("open_file", {
                          library: library(),
                          path: selected()!.path,
                        })
                  }
                >
                  {t("open")}
                </button>
                <button
                  aria-pressed={details()}
                  disabled={!selection()}
                  onClick={() => setDetails((value) => !value)}
                >
                  {t("details")}
                </button>
                <button
                  disabled={!ready() || !listing() || busy()}
                  onClick={() => {
                    showDialog(context("addMapping"));
                    setTarget(join(path(), ""));
                  }}
                >
                  {t("addMapping")}
                </button>
              </div>
            </div>
            <Show
              when={ready()}
              fallback={
                <div class="empty">
                  <h2>
                    {t(
                      state()?.system.available && !state()?.system.installed
                        ? "missingDaemon"
                        : "serviceUnavailable",
                    )}
                  </h2>
                  <p>
                    {t(
                      state()?.system.available && !state()?.system.installed
                        ? "installGuide"
                        : "authorization",
                    )}
                  </p>
                  <Show when={state()?.error}>
                    <p class="error">{errorText(state()?.error)}</p>
                  </Show>
                  <button onClick={() => setPage("status")}>
                    {t("status")}
                  </button>
                </div>
              }
            >
              <Show when={state()?.daemon && !state()!.daemon!.can_admin}>
                <div class="inline-note">{t("adminOnly")}</div>
              </Show>
              <div
                class="list-viewport"
                ref={viewport}
                aria-label={t("files")}
                aria-busy={loading()}
                tabIndex={0}
              >
                <table>
                  <thead>
                    <tr>
                      <th>{t("name")}</th>
                      <th>{t("type")}</th>
                      <th>{t("size")}</th>
                      <th>{t("modified")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    <For each={listing()?.entries ?? []}>
                      {(entry) => (
                        <tr
                          classList={{ selected: selected()?.id === entry.id }}
                          onClick={() => setSelection(entry)}
                          onDblClick={() =>
                            entry.kind === "directory"
                              ? void navigate(library(), entry.path)
                              : void perform("open_file", {
                                  library: library(),
                                  path: entry.path,
                                })
                          }
                          onContextMenu={(event) => {
                            event.preventDefault();
                            setSelection(entry);
                            setDetails(true);
                          }}
                        >
                          <td>
                            <button
                              class="filename"
                              title={entry.name}
                              onFocus={() => setSelection(entry)}
                              onKeyDown={(event) => {
                                if (event.key === "Enter") {
                                  event.preventDefault();
                                  entry.kind === "directory"
                                    ? void navigate(library(), entry.path)
                                    : void perform("open_file", {
                                        library: library(),
                                        path: entry.path,
                                      });
                                }
                              }}
                            >
                              <Icon kind={entry.kind} />
                              <span>{entry.name}</span>
                            </button>
                          </td>
                          <td>{t(entry.kind)}</td>
                          <td>
                            {entry.kind === "directory"
                              ? "—"
                              : entry.available
                                ? size(entry.size)
                                : t("unavailable")}
                          </td>
                          <td
                            title={entry.modified ? date(entry.modified) : ""}
                          >
                            {entry.modified ? date(entry.modified) : "—"}
                          </td>
                        </tr>
                      )}
                    </For>
                  </tbody>
                </table>
                <Show
                  when={!loading() && !error() && !listing()?.entries.length}
                >
                  <p class="empty-text">
                    {t(libraries().length ? "empty" : "noLibraries")}
                  </p>
                </Show>
                <Show when={loading()}>
                  <p class="empty-text">{t("loading")}</p>
                </Show>
              </div>
            </Show>
            <Show when={details() && selected()}>
              <section class="detail">
                <div class="detail-heading">
                  <strong title={selected()!.name}>{selected()!.name}</strong>
                  <button
                    class="dismiss"
                    aria-label={t("close")}
                    onClick={() => setDetails(false)}
                  >
                    ×
                  </button>
                </div>
                <dl>
                  <dt>{t("type")}</dt>
                  <dd>{t(selected()!.kind)}</dd>
                  <dt>{t("path")}</dt>
                  <dd>
                    {library()}:/{selected()!.path}
                  </dd>
                  <Show when={selected()?.source}>
                    <dt>{t("source")}</dt>
                    <dd>{selected()!.source}</dd>
                  </Show>
                </dl>
                <p class="muted">
                  {selected()?.kind === "mapping"
                    ? t("cached")
                    : selected()?.kind === "file"
                      ? t("external")
                      : ""}
                </p>
                <div class="actions">
                  <Show when={selected()?.kind === "directory"}>
                    <button
                      disabled={!admin() || busy()}
                      onClick={() =>
                        showDialog(context("folderActions", selected()))
                      }
                    >
                      {t("folderActions")}
                    </button>
                  </Show>
                  <Show when={selected()?.kind === "mapping"}>
                    <button
                      disabled={!ready() || busy()}
                      onClick={() =>
                        showDialog(context("moveMapping", selected()))
                      }
                    >
                      {t("move")}
                    </button>
                    <Show when={selected()?.can_rebind}>
                      <button
                        disabled={!ready() || busy()}
                        onClick={() =>
                          showDialog(context("rebind", selected()))
                        }
                      >
                        {t("rebind")}
                      </button>
                    </Show>
                    <button
                      class="danger"
                      disabled={!ready() || busy()}
                      onClick={() =>
                        showDialog(context("removeMapping", selected()))
                      }
                    >
                      {t("removeMapping")}
                    </button>
                  </Show>
                </div>
              </section>
            </Show>
          </section>
        </div>
      </section>
      <section class="fullpage" hidden={page() !== "status"}>
        <div class="page-heading">
          <h2>{t("status")}</h2>
          <button disabled={busy()} onClick={() => void updateState()}>
            {t("refresh")}
          </button>
        </div>
        <Show when={state()}>
          {(value) => (
            <>
              <dl class="summary">
                <dt>{t("service")}</dt>
                <dd>
                  {value().system.available
                    ? `${t(value().system.active)} / ${t(value().system.substate)}`
                    : t("unavailable")}
                </dd>
                <dt>{t("daemon")}</dt>
                <dd>{t(value().daemon?.daemon.ready ? "ready" : "stopped")}</dd>
                <dt>{t("version")}</dt>
                <dd>{value().daemon?.daemon.version ?? "—"}</dd>
                <For
                  each={
                    ["mount", "socket", "database", "object_store"] as const
                  }
                >
                  {(component) => (
                    <>
                      <dt>
                        {t(
                          {
                            mount: "mount",
                            socket: "socket",
                            database: "database",
                            object_store: "objectStore",
                          }[component],
                        )}
                      </dt>
                      <dd>
                        <span class="badge">
                          {t(
                            value().daemon?.[component].state ?? "unavailable",
                          )}
                        </span>{" "}
                        <code>{value().daemon?.[component].path ?? "—"}</code>
                      </dd>
                    </>
                  )}
                </For>
                <dt>{t("mappings")}</dt>
                <dd>{value().daemon?.namespace.projections ?? "—"}</dd>
                <dt>{t("files")}</dt>
                <dd>{value().daemon?.namespace.overlay_files ?? "—"}</dd>
              </dl>
              <Show
                when={
                  value().system.available &&
                  !value().system.installed &&
                  !value().daemon
                }
              >
                <div class="message">
                  <strong>{t("missingDaemon")}</strong>
                  <p>{t("installGuide")}</p>
                  <code>
                    docs/{language() === "zh" ? "zh-CN" : "en"}/deployment.md
                  </code>
                </div>
              </Show>
              <Show when={value().error && value().system.installed}>
                <div class="message error">{errorText(value().error)}</div>
              </Show>
              <p class="muted">
                {t(value().system_managed ? "authorization" : "customEndpoint")}
              </p>
              <div class="actions">
                <button
                  disabled={
                    busy() ||
                    !value().system_managed ||
                    !value().system.installed ||
                    ready()
                  }
                  onClick={() => void start()}
                >
                  {t("start")}
                </button>
                <button
                  disabled={
                    busy() ||
                    !value().system_managed ||
                    value().system.active !== "active"
                  }
                  onClick={() => showDialog({ kind: "stop" })}
                >
                  {t("stop")}
                </button>
                <button
                  disabled={
                    busy() ||
                    !value().system_managed ||
                    !value().system.installed
                  }
                  onClick={() => showDialog({ kind: "restart" })}
                >
                  {t("restart")}
                </button>
                <button
                  disabled={busy() || !ready()}
                  onClick={() => void diagnostics()}
                >
                  {t("check")}
                </button>
              </div>
            </>
          )}
        </Show>
        <div class="diagnostics">
          <h3>{t("check")}</h3>
          <p class="muted">
            {checkedAt()
              ? `${t("checkTime")} ${date(checkedAt()!)}`
              : t("notChecked")}
          </p>
          <Show when={doctor()}>
            {(report) => (
              <>
                <div class="actions">
                  <span>
                    {t("healthy")} {report().summary.healthy}
                  </span>
                  <span>
                    {t("warning")} {report().summary.warning}
                  </span>
                  <span>
                    {t("blocking")} {report().summary.blocking}
                  </span>
                </div>
                <ul class="findings">
                  <For each={report().findings}>
                    {(finding) => (
                      <li
                        classList={{ warning: finding.severity !== "healthy" }}
                      >
                        <span class="badge">{t(finding.severity)}</span>{" "}
                        {findingText(language(), finding.code)}{" "}
                        <code>
                          {finding.library}
                          {finding.target ? ` / ${finding.target}` : ""}
                        </code>
                      </li>
                    )}
                  </For>
                </ul>
              </>
            )}
          </Show>
        </div>
      </section>
      <section class="fullpage" hidden={page() !== "config"}>
        <div class="page-heading">
          <h2>{t("config")}</h2>
          <div class="actions">
            <button disabled={busy()} onClick={() => void loadConfig()}>
              {t("reloadConfig")}
            </button>
            <button
              disabled={busy() || !state()?.system_managed}
              onClick={() => void loadConfig(true)}
            >
              {t("authorizeRead")}
            </button>
          </div>
        </div>
        <p class="muted">{t("configNote")}</p>
        <Show when={configError()}>
          <div class="message error" role="alert">
            {errorText(configError())}
          </div>
        </Show>
        <Show when={document()}>
          {(saved) => (
            <>
              <div class="config-state">
                <span classList={{ warning: dirty() }}>
                  {dirty()
                    ? t("unsaved")
                    : state()?.daemon?.daemon.configuration_digest
                      ? t(
                          state()?.daemon?.daemon.configuration_digest ===
                            saved().effective_digest
                            ? "configCurrent"
                            : "pendingRestart",
                        )
                      : t("runningConfigUnknown")}
                </span>
              </div>
              <Show when={saved().validation_error}>
                <div class="message error">
                  {t("invalidConfig")}
                  <pre>{saved().validation_error}</pre>
                </div>
              </Show>
              <div class="actions">
                <button
                  aria-pressed={!raw()}
                  disabled={!!saved().validation_error || dirty()}
                  onClick={() => setRaw(false)}
                >
                  {t("form")}
                </button>
                <button
                  aria-pressed={raw()}
                  disabled={dirty()}
                  onClick={() => setRaw(true)}
                >
                  {t("raw")}
                </button>
              </div>
              <form class="config-form" onSubmit={previewConfig}>
                <div hidden={raw()} class="settings">
                  <For each={configKeys}>
                    {(field) => (
                      <label>
                        <span>{t(field.split(".")[1])}</span>
                        <input
                          value={fields()[field] ?? ""}
                          onInput={(event) => {
                            setFields((value) => ({
                              ...value,
                              [field]: event.currentTarget.value,
                            }));
                            setConfigPreview(undefined);
                          }}
                        />
                      </label>
                    )}
                  </For>
                </div>
                <textarea
                  hidden={!raw()}
                  class="raw-editor"
                  aria-label={t("raw")}
                  spellcheck={false}
                  value={rawText()}
                  onInput={(event) => {
                    setRawText(event.currentTarget.value);
                    setConfigPreview(undefined);
                  }}
                />
                <p class="muted">{t("backup")}</p>
                <button
                  type="submit"
                  disabled={busy() || !state()?.system_managed}
                >
                  {t("preview")}
                </button>
              </form>
              <Show when={configPreview()}>
                {(preview) => (
                  <section class="config-preview">
                    <h3>{t("preview")}</h3>
                    <Show
                      when={preview().changes.length}
                      fallback={<p>{t("noChanges")}</p>}
                    >
                      <table>
                        <thead>
                          <tr>
                            <th>{t("name")}</th>
                            <th>{t("before")}</th>
                            <th>{t("after")}</th>
                          </tr>
                        </thead>
                        <tbody>
                          <For each={preview().changes}>
                            {(change) => (
                              <tr>
                                <td>{t(change.key.split(".")[1])}</td>
                                <td>
                                  <code>{change.before}</code>
                                </td>
                                <td>
                                  <code>{change.after}</code>
                                </td>
                              </tr>
                            )}
                          </For>
                        </tbody>
                      </table>
                    </Show>
                    <Show when={preview().storage_changed}>
                      <div class="message">{t("storageWarning")}</div>
                    </Show>
                    <div class="actions">
                      <button
                        disabled={
                          busy() ||
                          preview().document.digest ===
                            preview().previous_digest
                        }
                        onClick={() => void saveConfig(false)}
                      >
                        {t("saveOnly")}
                      </button>
                      <button
                        class="primary"
                        disabled={
                          busy() ||
                          preview().document.digest ===
                            preview().previous_digest
                        }
                        onClick={() => void saveConfig(true)}
                      >
                        {t("saveRestart")}
                      </button>
                      <button
                        disabled={busy()}
                        onClick={() => setConfigPreview(undefined)}
                      >
                        {t("cancel")}
                      </button>
                    </div>
                  </section>
                )}
              </Show>
            </>
          )}
        </Show>
      </section>
      <footer aria-live="polite">
        <span>
          {busy()
            ? t("working")
            : notice()
              ? t(notice())
              : loading()
                ? t("loading")
                : listing() && page() === "libraries"
                  ? `${listing()!.entries.length} ${t("entries")}`
                  : "ScrapeFS"}
        </span>
        <Show when={page() === "libraries" && currentLibrary()}>
          <span>
            {currentLibrary()!.profile_name}@{currentLibrary()!.profile_version}
          </span>
        </Show>
      </footer>
      <dialog
        ref={modal}
        aria-labelledby="dialog-title"
        onKeyDown={(event) => {
          if (event.key === "Escape" && !busy()) {
            event.preventDefault();
            setDialog(undefined);
          }
        }}
        onCancel={(event) => {
          if (busy()) event.preventDefault();
          else setDialog(undefined);
        }}
        onClose={() => setDialog(undefined)}
      >
        <Show when={dialog()}>
          {(operation) => (
            <form onSubmit={submitDialog}>
              <div class="page-heading">
                <h2 id="dialog-title">
                  {t(
                    operation().kind === "moveMapping"
                      ? "move"
                      : operation().kind,
                  )}
                </h2>
                <button
                  type="button"
                  class="dismiss"
                  disabled={busy()}
                  aria-label={t("close")}
                  onClick={() => setDialog(undefined)}
                >
                  ×
                </button>
              </div>
              <Show when={operation().library}>
                <p class="dialog-path">
                  {operation().library}:/{operation().path}
                </p>
              </Show>
              <Show when={dialogError()}>
                <div class="message error" role="alert">
                  {errorText(dialogError())}
                </div>
              </Show>
              <Show when={operation().kind === "editLibrary"}>
                <dl class="summary">
                  <dt>{t("name")}</dt>
                  <dd>{operation().library}</dd>
                  <dt>{t("profile")}</dt>
                  <dd>{profile()}</dd>
                </dl>
                <div class="actions">
                  <button
                    type="button"
                    onClick={() =>
                      showDialog({ ...operation(), kind: "rename" })
                    }
                  >
                    {t("rename")}
                  </button>
                  <button
                    type="button"
                    onClick={() =>
                      showDialog({ ...operation(), kind: "changeProfile" })
                    }
                  >
                    {t("changeProfile")}
                  </button>
                  <button
                    type="button"
                    class="danger"
                    onClick={() =>
                      showDialog({
                        ...operation(),
                        kind: "removeLibrary",
                        path: "",
                      })
                    }
                  >
                    {t("removeLibrary")}
                  </button>
                </div>
              </Show>
              <Show when={operation().kind === "folderActions"}>
                <div class="actions">
                  <button
                    type="button"
                    onClick={() => showDialog({ ...operation(), kind: "move" })}
                  >
                    {t("move")}
                  </button>
                  <button
                    type="button"
                    class="danger"
                    onClick={() =>
                      showDialog({ ...operation(), kind: "removeFolder" })
                    }
                  >
                    {t("removeFolder")}
                  </button>
                </div>
              </Show>
              <Show
                when={["newLibrary", "rename", "newFolder"].includes(
                  operation().kind,
                )}
              >
                <label>
                  {t("newName")}
                  <input
                    required
                    autofocus
                    value={formName()}
                    onInput={(event) => setFormName(event.currentTarget.value)}
                    pattern="[^/\\]+"
                  />
                </label>
              </Show>
              <Show
                when={["newLibrary", "changeProfile"].includes(
                  operation().kind,
                )}
              >
                <label>
                  {t("profile")}
                  <select
                    value={profile()}
                    onChange={(event) => setProfile(event.currentTarget.value)}
                  >
                    <option value="jellyfin@1">jellyfin@1</option>
                    <option value="plex@1">plex@1</option>
                  </select>
                </label>
              </Show>
              <Show
                when={["move", "moveMapping", "addMapping"].includes(
                  operation().kind,
                )}
              >
                <label>
                  {t("target")}
                  <input
                    required
                    value={target()}
                    onInput={(event) => setTarget(event.currentTarget.value)}
                    placeholder="Movies/Film/film.mkv"
                  />
                </label>
              </Show>
              <Show when={["addMapping", "rebind"].includes(operation().kind)}>
                <label>
                  {t("source")}
                  <div class="input-action">
                    <input
                      required
                      aria-label={t("source")}
                      disabled={busy()}
                      value={source()}
                      onInput={(event) => {
                        setSource(event.currentTarget.value);
                        setRebind(undefined);
                      }}
                    />
                    <button type="button" onClick={() => void chooseSource()}>
                      {t("choose")}
                    </button>
                  </div>
                </label>
              </Show>
              <Show when={operation().kind === "rebind"}>
                <p class="muted">{t("rebindNote")}</p>
                <Show when={rebind()}>
                  <div class="message">
                    <code>{rebind()!.proposed_source.path}</code>
                    <p>
                      {t("size")}: {size(rebind()!.proposed_source.size)}
                    </p>
                  </div>
                </Show>
              </Show>
              <Show
                when={["removeFolder", "removeLibrary"].includes(
                  operation().kind,
                )}
              >
                <p class="message">
                  {t(
                    operation().kind === "removeLibrary"
                      ? "libraryWarning"
                      : "removalWarning",
                  )}
                </p>
                <Show
                  when={removal()}
                  fallback={
                    <button
                      type="button"
                      disabled={busy()}
                      onClick={() => void previewRemoval()}
                    >
                      {t("previewRemoval")}
                    </button>
                  }
                >
                  {(preview) => (
                    <>
                      <dl class="summary">
                        <dt>{t("directories")}</dt>
                        <dd>{preview().directories}</dd>
                        <dt>{t("mappings")}</dt>
                        <dd>{preview().mappings}</dd>
                        <dt>{t("files")}</dt>
                        <dd>{preview().files}</dd>
                      </dl>
                      <Show when={preview().unavailable}>
                        <p class="message">{t("missingFiles")}</p>
                      </Show>
                      <button type="submit" class="danger" disabled={busy()}>
                        {t("confirmDelete")}
                      </button>
                    </>
                  )}
                </Show>
              </Show>
              <Show when={operation().kind === "removeMapping"}>
                <p class="message">{t("keepSource")}</p>
                <button type="submit" class="danger" disabled={busy()}>
                  {t("confirmRemove")}
                </button>
              </Show>
              <Show when={["stop", "restart"].includes(operation().kind)}>
                <p class="message">
                  {t(
                    operation().kind === "stop"
                      ? "stopWarning"
                      : "restartWarning",
                  )}
                </p>
                <button type="submit" class="primary" disabled={busy()}>
                  {t("confirm")}
                </button>
              </Show>
              <Show
                when={[
                  "newLibrary",
                  "rename",
                  "changeProfile",
                  "newFolder",
                  "move",
                  "moveMapping",
                  "addMapping",
                  "rebind",
                ].includes(operation().kind)}
              >
                <div class="actions">
                  <button type="submit" class="primary" disabled={busy()}>
                    {t(
                      operation().kind === "rebind"
                        ? rebind()
                          ? "confirmRebind"
                          : "preview"
                        : "confirm",
                    )}
                  </button>
                  <button
                    type="button"
                    disabled={busy()}
                    onClick={() => setDialog(undefined)}
                  >
                    {t("cancel")}
                  </button>
                </div>
              </Show>
            </form>
          )}
        </Show>
      </dialog>
    </div>
  );
}

render(() => <App />, window.document.getElementById("root")!);
