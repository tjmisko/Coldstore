import type { FileRecord } from "../types/api";
import { formatCoordinates } from "../lib/location";

interface LocationMetaProps {
  file: FileRecord;
}

export function LocationMeta({ file }: LocationMetaProps) {
  if (file.gps_lat === null || file.gps_lon === null) {
    return <span data-location="unknown">Not recorded</span>;
  }
  return (
    <span data-location={`${file.gps_lat},${file.gps_lon}`} className="tabular">
      {formatCoordinates(file.gps_lat, file.gps_lon)}
      {file.gps_accuracy_m !== undefined && ` (±${file.gps_accuracy_m.toFixed(1)} m)`}
      {file.gps_altitude_m !== undefined && ` · ${file.gps_altitude_m.toFixed(1)} m altitude`}
      {file.gps_source === "manual" && " · manual"}
    </span>
  );
}
