const paths = {
  open: "M14 3h7v7m0-7L11 13M10 3H5a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-5",
  rename: "m15 5 4 4M4 20l4-1L20 7a2.8 2.8 0 0 0-4-4L4 15v5Z",
  delete: "M3 6h18M9 6V3h6v3M5 6l1 15h12l1-15M10 10v7m4-7v7",
  profile: "M4 6h16M7 3v6M4 12h16m-4-3v6M4 18h16m-10-3v6",
  rebind: "M3 7h16m-4-4 4 4-4 4M21 17H5m4-4-4 4 4 4",
} as const;

export function ActionIcon(props: { kind: keyof typeof paths }) {
  return (
    <svg
      viewBox="0 0 24 24"
      width="14"
      height="14"
      fill="none"
      stroke="currentColor"
      stroke-width="1.6"
      stroke-linecap="round"
      stroke-linejoin="round"
      aria-hidden="true"
    >
      <path d={paths[props.kind]} />
    </svg>
  );
}
