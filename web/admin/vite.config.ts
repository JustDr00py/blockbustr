import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Built into internal/adminui/dist and embedded in the binary, served at
// /blockbustr/ui/ (TASKS P4.3). `npm run dev` proxies the API to a local
// blockbustr.
export default defineConfig({
  base: "/blockbustr/ui/",
  plugins: [react()],
  build: { outDir: "../../internal/adminui/dist", emptyOutDir: true },
  server: {
    proxy: {
      "^/(Users|Library|Sessions|System|blockbustr/(addons|debrid|catalogs))": "http://localhost:8096",
    },
  },
});
