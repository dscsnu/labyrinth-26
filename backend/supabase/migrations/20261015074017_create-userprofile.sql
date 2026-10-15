CREATE TYPE "UserRole" AS ENUM ('PLAYER', 'HELPER', 'ADMIN');

CREATE TABLE "userprofile" (
    "id" UUID NOT NULL,
    "name" TEXT NOT NULL,
    "email" TEXT NOT NULL,
    "role" "UserRole" NOT NULL DEFAULT 'PLAYER',
    "created_at" TIMESTAMP(3) NOT NULL DEFAULT now(),

    CONSTRAINT "userprofile_pkey" PRIMARY KEY ("id")
);

CREATE UNIQUE INDEX "userprofile_email_key" ON "userprofile"("email");


-- Trigger: on new auth user -> create profile row + set default custom claim
CREATE OR REPLACE FUNCTION public.fn_on_auth_new_user()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
BEGIN
    INSERT INTO public.userprofile (id, name, email)
    VALUES (new.id, '', '');

    UPDATE public.userprofile SET
        name = COALESCE(
            new.raw_user_meta_data->>'full_name',
            new.raw_user_meta_data->>'name',
            ''
        ),
        email = COALESCE(new.email, '')
    WHERE id = new.id;

    UPDATE auth.users SET
        raw_app_meta_data = jsonb_set(
            COALESCE(raw_app_meta_data, '{}'::jsonb),
            '{custom_claims}',
            '{"role": "PLAYER"}'::jsonb,
            true
        )
    WHERE id = new.id;

    RETURN NEW;
END;
$$;

CREATE OR REPLACE TRIGGER tr_on_auth_new_user
    AFTER INSERT ON auth.users
    FOR EACH ROW EXECUTE PROCEDURE public.fn_on_auth_new_user();


-- Trigger: role changed on userprofile -> refresh custom claims in auth.users
CREATE OR REPLACE FUNCTION public.fn_on_user_role_update()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
BEGIN
    UPDATE auth.users
    SET raw_app_meta_data = jsonb_set(
        COALESCE(raw_app_meta_data, '{}'::jsonb),
        '{custom_claims}',
        jsonb_build_object('role', NEW.role),
        true
    )
    WHERE id = NEW.id;

    RETURN NEW;
END;
$$;

CREATE OR REPLACE TRIGGER tr_on_user_role_update
    AFTER UPDATE OF role ON public.userprofile
    FOR EACH ROW
    WHEN (OLD.role IS DISTINCT FROM NEW.role)
    EXECUTE PROCEDURE public.fn_on_user_role_update();


-- Trigger: auth user deleted -> cascade delete the profile row
CREATE OR REPLACE FUNCTION public.fn_on_auth_delete_user()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $$
BEGIN
    DELETE FROM public.userprofile WHERE id = old.id;
    RETURN old;
END;
$$;

CREATE OR REPLACE TRIGGER tr_on_auth_delete_user
    AFTER DELETE ON auth.users
    FOR EACH ROW EXECUTE PROCEDURE public.fn_on_auth_delete_user();