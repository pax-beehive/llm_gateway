import { useEffect } from "react";
import { AuthProvider, useAuth } from "./auth/AuthProvider";
import { RequireSession } from "./auth/RequireSession";
import { Layout, NAV } from "./components/layout";
import { ToastProvider } from "./components/feedback";
import { navigate, useRoute } from "./router";
import OverviewPage from "./pages/overview";
import PlaygroundPage from "./pages/playground";
import ModelsPage from "./pages/models";
import TenantsPage from "./pages/tenants";
import ProvidersPage from "./pages/providers";
import RoutingPage from "./pages/routing";
import UsagePage from "./pages/usage";
import OperationsPage from "./pages/operations";

const PAGES: Record<string, () => JSX.Element> = {
  overview: OverviewPage,
  playground: PlaygroundPage,
  models: ModelsPage,
  tenants: TenantsPage,
  providers: ProvidersPage,
  routing: RoutingPage,
  usage: UsagePage,
  operations: OperationsPage,
};

function firstAccessibleRoute(can: (permission: string) => boolean): string {
  return NAV.find((n) => !n.permission || can(n.permission))?.id ?? "overview";
}

/**
 * Renders the active page inside the Layout. When the session's permissions
 * change and the current route loses its read permission, navigates to the
 * first accessible section instead of rendering a forbidden page.
 */
function GuardedConsole() {
  const route = useRoute();
  const { can } = useAuth();
  const def = NAV.find((n) => n.id === route);
  const allowed = !def?.permission || can(def.permission);

  useEffect(() => {
    if (!allowed && route !== firstAccessibleRoute(can)) {
      navigate(firstAccessibleRoute(can));
    }
  }, [allowed, route, can]);

  const effective = allowed ? route : firstAccessibleRoute(can);
  const Page = PAGES[effective] ?? OverviewPage;
  return (
    <Layout>
      <Page />
    </Layout>
  );
}

export default function App() {
  return (
    <ToastProvider>
      <AuthProvider>
        <RequireSession>
          <GuardedConsole />
        </RequireSession>
      </AuthProvider>
    </ToastProvider>
  );
}
