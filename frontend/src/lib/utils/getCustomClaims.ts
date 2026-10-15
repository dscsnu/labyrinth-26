import type { CustomClaims } from "#lib/types/CustomClaims.type.ts";
import type { User } from "@supabase/supabase-js";

const getCustomClaims = (user: User | null): CustomClaims | null => {
    if (!user) return null;
    return user.app_metadata.custom_claims as unknown as CustomClaims;
}

export default getCustomClaims;