export type ContextMenuIconName =
  | "open"
  | "rename"
  | "description"
  | "location"
  | "show-in-files"
  | "add-tags"
  | "remove-tags"
  | "download"
  | "restore"
  | "archive"
  | "trash"
  | "delete-permanently"
  | "pin"
  | "unpin"
  | "resolve-device"
  | "resolve-archive"
  | "copy-path";

export function ContextMenuIcon({ name }: { name: ContextMenuIconName }) {
  const paths = (() => {
    switch (name) {
      case "open":
        return <><path d="M5 12h11" /><path d="m12 8 4 4-4 4" /><path d="M19 5H7a2 2 0 0 0-2 2v10a2 2 0 0 0 2 2h12" /></>;
      case "rename":
        return <><path d="m14 5 5 5" /><path d="M4 20l3.5-.8L19 7.7a1.8 1.8 0 0 0 0-2.5l-.2-.2a1.8 1.8 0 0 0-2.5 0L4.8 16.5 4 20Z" /></>;
      case "description":
        return <><path d="M6 4h9l3 3v13H6z" /><path d="M15 4v4h3M9 12h6M9 16h5" /></>;
      case "location":
        return <><path d="M12 21s6-5.2 6-11a6 6 0 1 0-12 0c0 5.8 6 11 6 11Z" /><circle cx="12" cy="10" r="2" /></>;
      case "show-in-files":
        return <><path d="M3 7.5h7l2-2h9v13H3z" /><path d="m12 10 3 3-3 3M7 13h8" /></>;
      case "add-tags":
      case "remove-tags":
        return <><path d="M4 5h7l8 8-6 6-8-8z" /><circle cx="8" cy="9" r="1" /><path d={name === "add-tags" ? "M14 6v5M11.5 8.5h5" : "M11.5 8.5h5"} /></>;
      case "download":
        return <><path d="M12 3v12m0 0 4-4m-4 4-4-4" /><path d="M5 20h14" /></>;
      case "restore":
        return <><path d="M5 8H3V3" /><path d="M4 7a8 8 0 1 1-1 8" /><path d="m9 12 2 2 4-4" /></>;
      case "archive":
        return <><path d="M4 8h16v12H4zM3 4h18v4H3zM9 12h6" /></>;
      case "trash":
      case "delete-permanently":
        return <><path d="M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13M10 11v5M14 11v5" />{name === "delete-permanently" && <path d="m9 11 6 6m0-6-6 6" />}</>;
      case "pin":
      case "unpin":
        return <><path d="m9 4 6 6-2 2 3 4-1 1-4-3-2 2-6-6zM8 15l-4 5" />{name === "unpin" && <path d="M4 4l16 16" />}</>;
      case "resolve-device":
        return <><rect x="3" y="4" width="18" height="13" rx="2" /><path d="M8 21h8M12 17v4m-3-10 2 2 4-4" /></>;
      case "resolve-archive":
        return <><path d="M4 8h16v12H4zM3 4h18v4H3z" /><path d="m9 14 2 2 4-4" /></>;
      case "copy-path":
        return <><rect x="8" y="8" width="11" height="12" rx="2" /><path d="M16 8V6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v10a2 2 0 0 0 2 2h2" /></>;
    }
  })();

  return (
    <svg
      aria-hidden="true"
      data-menu-icon={name}
      className="size-4 shrink-0"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.7"
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      {paths}
    </svg>
  );
}
