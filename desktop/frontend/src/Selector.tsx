import {
  For,
  Show,
  createEffect,
  createSignal,
  createUniqueId,
  onCleanup,
  onMount,
} from "solid-js";
import "./Selector.css";

export interface SelectorOption {
  value: string;
  label: string;
}

export function Selector(props: {
  value: string;
  options: readonly SelectorOption[];
  onChange: (value: string) => void;
  label: string;
  id?: string;
  class?: string;
  disabled?: boolean;
}) {
  const instance = createUniqueId();
  const listID = `${instance}-list`;
  const [open, setOpen] = createSignal(false);
  const [active, setActive] = createSignal(0);
  const [above, setAbove] = createSignal(false);
  const [menuHeight, setMenuHeight] = createSignal(240);
  let root!: HTMLDivElement;
  let trigger!: HTMLButtonElement;
  let list!: HTMLUListElement;
  let search = "";
  let searchedAt = 0;
  const disabled = () => !!props.disabled || !props.options.length;
  const selectedIndex = () =>
    Math.max(
      0,
      props.options.findIndex((option) => option.value === props.value),
    );
  const optionID = (index: number) => `${instance}-option-${index}`;

  function expand(index = selectedIndex()) {
    if (disabled()) return;
    setActive(index);
    setOpen(true);
    search = "";
  }
  function close(commit = false) {
    const option = props.options[active()];
    if (open() && commit && option && option.value !== props.value)
      props.onChange(option.value);
    setOpen(false);
    search = "";
  }
  function positionMenu() {
    if (!open()) return;
    const bounds = root.closest("dialog")?.getBoundingClientRect();
    const box = trigger.getBoundingClientRect();
    const below = (bounds?.bottom ?? window.innerHeight) - box.bottom - 6;
    const aboveSpace = box.top - (bounds?.top ?? 0) - 6;
    const upward =
      below < Math.min(240, list.scrollHeight) && aboveSpace > below;
    setAbove(upward);
    setMenuHeight(Math.max(24, Math.min(240, upward ? aboveSpace : below)));
  }
  function keyDown(event: KeyboardEvent) {
    if (disabled()) return;
    switch (event.key) {
      case "ArrowDown":
      case "ArrowUp": {
        event.preventDefault();
        if (!open()) expand(event.key === "ArrowUp" ? 0 : selectedIndex());
        else
          setActive((index) =>
            Math.max(
              0,
              Math.min(
                props.options.length - 1,
                index + (event.key === "ArrowDown" ? 1 : -1),
              ),
            ),
          );
        return;
      }
      case "Home":
      case "End":
        event.preventDefault();
        expand(event.key === "Home" ? 0 : props.options.length - 1);
        return;
      case "Enter":
      case " ":
        event.preventDefault();
        if (open()) close(true);
        else expand();
        return;
      case "Escape":
        if (open()) {
          event.preventDefault();
          event.stopPropagation();
          close();
        }
        return;
      case "Tab":
        close(true);
        return;
    }
    if (
      event.key.length !== 1 ||
      event.ctrlKey ||
      event.metaKey ||
      event.altKey
    )
      return;
    event.preventDefault();
    const now = Date.now();
    const character = event.key.toLocaleLowerCase();
    const previous = now - searchedAt < 700 ? search : "";
    search = previous + character;
    searchedAt = now;
    const repeated = [...search].every((part) => part === character);
    const prefix = repeated ? character : search;
    const start = open() ? active() : selectedIndex();
    for (
      let offset = previous && !repeated ? 0 : 1;
      offset <= props.options.length;
      offset++
    ) {
      const index = (start + offset) % props.options.length;
      if (props.options[index].label.toLocaleLowerCase().startsWith(prefix)) {
        setActive(index);
        setOpen(true);
        break;
      }
    }
  }

  createEffect(() => {
    const selected = selectedIndex();
    if (disabled()) setOpen(false);
    if (!open()) setActive(selected);
    else if (active() >= props.options.length)
      setActive(props.options.length - 1);
  });
  createEffect(() => {
    if (!open()) return;
    const index = active();
    queueMicrotask(() => {
      if (!open() || active() !== index) return;
      positionMenu();
      document
        .getElementById(optionID(index))
        ?.scrollIntoView({ block: "nearest" });
    });
  });
  onMount(() => {
    const outside = (event: PointerEvent) => {
      if (!root.contains(event.target as Node)) close(true);
    };
    document.addEventListener("pointerdown", outside);
    window.addEventListener("resize", positionMenu);
    onCleanup(() => {
      document.removeEventListener("pointerdown", outside);
      window.removeEventListener("resize", positionMenu);
    });
  });

  return (
    <div
      ref={root}
      class={`selector${props.class ? ` ${props.class}` : ""}`}
      classList={{ "selector-above": above() }}
      onFocusOut={(event) => {
        if (!root.contains(event.relatedTarget as Node | null)) close(true);
      }}
    >
      <button
        ref={trigger}
        id={props.id}
        type="button"
        class="selector-trigger"
        role="combobox"
        aria-label={props.label}
        aria-haspopup="listbox"
        aria-controls={listID}
        aria-expanded={open()}
        aria-activedescendant={open() ? optionID(active()) : undefined}
        disabled={disabled()}
        onClick={() => (open() ? close() : expand())}
        onKeyDown={keyDown}
      >
        <span class="selector-value">
          {props.options.find((option) => option.value === props.value)
            ?.label ?? ""}
        </span>
        <svg viewBox="0 0 12 12" aria-hidden="true">
          <path
            d="m3 4 3 3 3-3"
            fill="none"
            stroke="currentColor"
            stroke-width="1.4"
          />
        </svg>
      </button>
      <Show when={open()}>
        <ul
          ref={list}
          id={listID}
          class="selector-list"
          role="listbox"
          aria-label={props.label}
          style={{ "max-height": `${menuHeight()}px` }}
        >
          <For each={props.options}>
            {(option, index) => (
              <li
                id={optionID(index())}
                class="selector-option"
                classList={{
                  "is-active": active() === index(),
                  "is-selected": option.value === props.value,
                }}
                role="option"
                aria-selected={option.value === props.value}
                onPointerDown={(event) => event.preventDefault()}
                onClick={(event) => {
                  event.preventDefault();
                  setActive(index());
                  close(true);
                  trigger.focus();
                }}
              >
                <span class="selector-check" aria-hidden="true">
                  {option.value === props.value ? "✓" : ""}
                </span>
                <span>{option.label}</span>
              </li>
            )}
          </For>
        </ul>
      </Show>
    </div>
  );
}
