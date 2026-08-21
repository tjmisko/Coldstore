import { useMemo, useRef, useState } from "react";
import type { FileActions } from "../hooks/useFileWorkspace";
import type { NearFilter } from "../lib/api";
import { formatCoordinates } from "../lib/location";
import type { FileRecord } from "../types/api";
import { Overlay } from "./Overlay";

interface LocationEditorProps {
  file: FileRecord;
  actions: FileActions;
  onFindNearby(near: NearFilter): void;
  onClose(): void;
}

interface ParsedLocation {
  location?: { lat: number; lon: number };
  radiusKm?: number;
  error?: string;
}

function parseLocation(latText: string, lonText: string, radiusText: string): ParsedLocation {
  if (latText.trim() === "" || lonText.trim() === "") {
    return { error: "Latitude and longitude are both required." };
  }
  const lat = Number(latText);
  const lon = Number(lonText);
  const radiusKm = Number(radiusText);
  if (!Number.isFinite(lat) || !Number.isFinite(lon)) {
    return { error: "Coordinates must be finite numbers." };
  }
  if (lat < -90 || lat > 90) return { error: "Latitude must be between -90 and 90." };
  if (lon < -180 || lon > 180) return { error: "Longitude must be between -180 and 180." };
  if (!Number.isFinite(radiusKm) || radiusKm <= 0 || radiusKm > 500) {
    return { location: { lat, lon }, error: "Radius must be greater than 0 and at most 500 km." };
  }
  return { location: { lat, lon }, radiusKm };
}

export function LocationEditor({ file, actions, onFindNearby, onClose }: LocationEditorProps) {
  const [lat, setLat] = useState(file.gps_lat === null ? "" : String(file.gps_lat));
  const [lon, setLon] = useState(file.gps_lon === null ? "" : String(file.gps_lon));
  const [radius, setRadius] = useState("25");
  const [busy, setBusy] = useState(false);
  const [submitted, setSubmitted] = useState(false);
  const firstInput = useRef<HTMLInputElement>(null);
  const parsed = useMemo(() => parseLocation(lat, lon, radius), [lat, lon, radius]);

  const save = async () => {
    setSubmitted(true);
    if (!parsed.location || parsed.error) return;
    setBusy(true);
    const ok = await actions.setLocation(file, parsed.location.lat, parsed.location.lon);
    setBusy(false);
    if (ok) onClose();
  };

  return (
    <Overlay label={`Location for ${file.filename}`} onClose={onClose} className="max-w-lg" testAttribute="location">
      <section className="rounded-xl border border-line-strong bg-surface p-5 shadow-2xl">
        <header className="mb-4">
          <h2 className="truncate text-base font-semibold text-ink">Location for {file.filename}</h2>
          <p className="mt-1 text-meta text-ink-faint">
            WGS84 decimal coordinates. Coldstore stores them locally and does not contact a map or geocoding service.
          </p>
        </header>
        <div className="grid gap-3 sm:grid-cols-2" data-keymap-ignore>
          <label className="text-meta text-ink-muted">
            Latitude
            <input
              ref={firstInput}
              autoFocus
              aria-label="Latitude"
              inputMode="decimal"
              value={lat}
              onChange={(event) => setLat(event.target.value)}
              placeholder="-90 to 90"
              className="mt-1 w-full rounded-lg border border-line-strong bg-ground px-3 py-2 text-base text-ink outline-none focus:border-cursor focus:ring-2 focus:ring-cursor/40"
            />
          </label>
          <label className="text-meta text-ink-muted">
            Longitude
            <input
              aria-label="Longitude"
              inputMode="decimal"
              value={lon}
              onChange={(event) => setLon(event.target.value)}
              placeholder="-180 to 180"
              className="mt-1 w-full rounded-lg border border-line-strong bg-ground px-3 py-2 text-base text-ink outline-none focus:border-cursor focus:ring-2 focus:ring-cursor/40"
            />
          </label>
        </div>
        <label className="mt-3 block text-meta text-ink-muted" data-keymap-ignore>
          Nearby radius (km)
          <input
            aria-label="Nearby radius in kilometres"
            inputMode="decimal"
            value={radius}
            onChange={(event) => setRadius(event.target.value)}
            className="mt-1 w-full rounded-lg border border-line-strong bg-ground px-3 py-2 text-base text-ink outline-none focus:border-cursor focus:ring-2 focus:ring-cursor/40"
          />
        </label>
        <p className="mt-2 text-meta text-ink-faint">
          Show nearby uses the values above as its centre without changing this file. Save coordinates writes them to the catalog.
        </p>
        {submitted && parsed.error && <p role="alert" className="mt-3 text-meta text-alarm">{parsed.error}</p>}
        {parsed.location && !parsed.error && (
          <p className="mt-3 text-meta text-ink-faint">Centre: {formatCoordinates(parsed.location.lat, parsed.location.lon)}</p>
        )}
        <footer className="mt-5 flex flex-wrap justify-end gap-2">
          <button type="button" onClick={onClose} className="rounded-md px-3 py-1.5 text-ink-muted ring-1 ring-line-strong hover:bg-raised-hover">
            Cancel
          </button>
          <button
            type="button"
            disabled={busy}
            onClick={() => {
              setSubmitted(true);
              if (parsed.location && parsed.radiusKm !== undefined && !parsed.error) {
                onFindNearby({ ...parsed.location, radiusKm: parsed.radiusKm });
              }
            }}
            className="rounded-md bg-raised px-3 py-1.5 text-ink ring-1 ring-line-strong hover:bg-raised-hover disabled:opacity-40"
          >
            Show nearby
          </button>
          <button
            type="button"
            disabled={busy}
            onClick={() => void save()}
            className="rounded-md bg-ink px-3 py-1.5 font-medium text-ground hover:brightness-95 disabled:opacity-40"
          >
            {busy ? "Saving…" : "Save coordinates"}
          </button>
        </footer>
      </section>
    </Overlay>
  );
}
