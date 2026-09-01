import { Layout } from "./components/layout";
import { ToastProvider } from "./components/feedback";
import { useRoute } from "./router";
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

export default function App() {
  const route = useRoute();
  const Page = PAGES[route] ?? OverviewPage;
  return (
    <ToastProvider>
      <Layout>
        <Page />
      </Layout>
    </ToastProvider>
  );
}
