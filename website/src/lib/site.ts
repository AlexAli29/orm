/*
  The canonical origin, in one place.

  It is used by the page metadata and by scripts/build-llms.mjs, which writes
  absolute URLs into llms.txt because the file is read away from the site that
  served it. Two copies of this string is how a documentation site comes to
  advertise a domain it no longer answers on.
*/
export const SITE_URL = "https://ormgo.vercel.app";
