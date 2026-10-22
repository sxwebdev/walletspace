import { QueryClient } from "@tanstack/react-query"

import { APIError } from "@/lib/api/client"

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // A refused request (wrong password, locked space, missing token) does
      // not get better by asking again; only network trouble is retried.
      retry: (count, error) =>
        count < 2 && !(error instanceof APIError && error.status < 500),
      refetchOnWindowFocus: false,
      staleTime: 15_000,
    },
  },
})
