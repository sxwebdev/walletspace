import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import { CSPProvider } from "@base-ui/react/csp-provider"
import { QueryClientProvider } from "@tanstack/react-query"
import { BrowserRouter } from "react-router"

import "./index.css"
import { App } from "@/app"
import { ThemeProvider } from "@/components/theme-provider"
import { Toaster } from "@/components/ui/toast"
import { TooltipProvider } from "@/components/ui/tooltip"
import { queryClient } from "@/lib/query"

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    {/* The wallet's CSP refuses inline <style>; index.css carries the rules
        Base UI would otherwise inject. */}
    <CSPProvider disableStyleElements>
      <ThemeProvider>
        <QueryClientProvider client={queryClient}>
          <TooltipProvider delay={300}>
            <Toaster>
              <BrowserRouter>
                <App />
              </BrowserRouter>
            </Toaster>
          </TooltipProvider>
        </QueryClientProvider>
      </ThemeProvider>
    </CSPProvider>
  </StrictMode>
)
