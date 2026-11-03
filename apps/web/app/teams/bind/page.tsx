"use client";

import { Suspense } from "react";
import { useSearchParams } from "next/navigation";
import { TeamsBindPage } from "@multica/views/teams";

// /teams/bind?token=<raw> is the bot's "link your account" destination.
// Suspense wraps useSearchParams per Next.js 15's CSR-bailout rule.
function TeamsBindPageContent() {
  const searchParams = useSearchParams();
  const token = searchParams.get("token");
  return <TeamsBindPage token={token} />;
}

export default function Page() {
  return (
    <Suspense fallback={null}>
      <TeamsBindPageContent />
    </Suspense>
  );
}
