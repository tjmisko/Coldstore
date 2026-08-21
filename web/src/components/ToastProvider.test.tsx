import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it } from "vitest";
import { useToast } from "../lib/toast";
import { Z } from "../lib/zLayers";
import { ConfirmDialog } from "./ConfirmDialog";
import { ToastProvider } from "./ToastProvider";

function ReportFailure() {
  const toast = useToast();
  return <button onClick={() => toast.show("error", "Action failed")}>Report failure</button>;
}

it("keeps action failures above every overlay layer", async () => {
  render(
    <ToastProvider>
      <ReportFailure />
      <ConfirmDialog
        title="Confirm action"
        confirmLabel="Continue"
        onConfirm={() => {}}
        onCancel={() => {}}
      >
        This dialog is the highest overlay.
      </ConfirmDialog>
    </ToastProvider>,
  );

  await userEvent.click(screen.getByRole("button", { name: "Report failure" }));

  const dialogLayer = screen.getByRole("dialog").parentElement;
  const toastLayer = screen.getByRole("alert").parentElement;
  expect(dialogLayer).toHaveStyle({ zIndex: String(Z.dialog) });
  expect(toastLayer).toHaveStyle({ zIndex: String(Z.toast) });
  expect(Z.toast).toBeGreaterThan(Math.max(Z.lightbox, Z.overlay, Z.contextMenu, Z.dialog));
  expect(Number(toastLayer?.style.zIndex)).toBeGreaterThan(Number(dialogLayer?.style.zIndex));
});
