# Modèle de menace d’Temper

État initial au 30 août 2026. Ce document décrit les frontières à protéger; il ne prétend pas que toutes les mesures sont déjà mises en œuvre.

## Actifs

- code et documents privés présents dans les transcripts;
- secrets, tokens, clés, URLs internes et données personnelles;
- instructions persistantes consommées par Claude Code, Codex et Cursor;
- registry, base SQLite, journal et sauvegardes;
- fichiers utilisateur situés hors des sections gérées;
- budget et endpoint LLM configurés par l’utilisateur.

## Adversaires et défaillances

- transcript contenant une prompt injection, volontaire ou issue d’un log/code externe;
- secret présent dans une transcript, un fichier de règles ou une configuration;
- réponse provider malformée, hostile ou simplement erronée;
- page web locale malveillante qui tente une requête vers l’API de review;
- base corrompue, panne entre écriture filesystem et transaction SQLite;
- substitution de chemin, junction, symlink ou casing ambigu;
- migration ou rollback incomplet;
- commande présente dans une transcript et interprétée comme instruction.

## Frontières de confiance

| Entrée ou sortie | Niveau | Règle |
|---|---|---|
| Transcripts et contenu de code/log | Non fiable | Parser comme données. Ne jamais exécuter. Neutraliser les marqueurs de contrôle. |
| Contexte AGENTS/CLAUDE/Cursor | Sensible | Redacter avant tout egress; borner le volume et la portée. |
| Provider LLM et sa réponse | Non fiable | Utiliser TLS pour le distant; valider schéma, taille, provenance et destinations. |
| Review UI locale | Autorité utilisateur | Loopback strict par défaut; contrôler Host/Origin, taille et délais; capability explicite si exposition. |
| Registry et store | Autorité locale | Transactions, version de schéma, integrity check, sauvegarde et journal. |
| Writers | Mutation sensible | Racine autorisée dont l’identité est capturée puis reprouvée, plan complet, temp+fsync fichier+rename, diff et rollback. |

## Invariants

1. Aucune donnée ne quitte la machine avant redaction et policy outbound.
2. Une transcript ne peut pas modifier le control plane par son contenu.
3. Une réponse LLM reste une proposition jusqu’à validation et review autorisée.
4. Un bloc shell proposé ne devient jamais un fichier exécutable sans autorisation dédiée, distincte de l’acceptation du texte.
5. Une mutation ne touche que les racines et sections explicitement gérées.
6. Le store, le journal et le filesystem convergent après succès comme après interruption du processus, à la reprise. La coupure d’alimentation matérielle n’est pas couverte: voir « Durabilité: ce qui est prouvé, et ce qui ne l’est pas ».
7. Le rollback restaure l’état précédent exact ou échoue sans détruire davantage.
8. Le daemon n’élargit pas l’autorité du CLI.

## Mesures requises

### Avant egress

- redacter clés API, bearer tokens, private keys, connection strings, credentials cloud, `.env`, emails et URLs internes selon la policy;
- limiter taille, nombre d’éléments et champs envoyés;
- refuser un endpoint distant sans TLS sauf opt-in local explicite;
- journaliser les compteurs, jamais les secrets ni le payload brut.

### Après réponse provider

- décoder un schéma fermé avec limites de taille et enums;
- vérifier que chaque preuve appartient aux données autorisées;
- recalculer sensibilité, confiance et destination côté local;
- supprimer l’auto-accept actuel; une exception future exigera une policy locale déterministe et restera interdite au contenu exécutable;
- présenter le plan complet des fichiers et le diff avant mutation.

### Pendant une mutation

- résoudre les chemins canoniques et vérifier leur confinement;
- capturer l’identité de l’**objet** de chaque racine autorisée et la reprouver avant toute lecture, écriture ou reprise — un nom auquel répond un autre répertoire n’est plus une autorité, et un handle déjà ouvert ne dispense pas de la preuve;
- préparer toutes les écritures dans le même filesystem;
- enregistrer manifeste, preimages, permissions et checksums dans une saga `prepared → applying → rolling_back? → committed | rolled_back`;
- reprouver chaque destination immédiatement avant son remplacement, puis le manifeste entier avant la décision: une preuve globale est déjà périmée après la première écriture;
- remplacer chaque fichier atomiquement et rendre chaque étape rejouable;
- rendre durable l’intention d’abandon **avant** de restaurer, pour qu’un rollback interrompu reprenne comme rollback;
- réconcilier au démarrage toute saga incomplète, puis terminer ou restaurer;
- n’avancer les checkpoints qu’après la persistance durable des objets qu’ils couvrent.

## Tests adversariaux minimaux

- clés OpenAI/Anthropic/GitHub, tokens AWS/GCP/Azure et private keys;
- URL avec utilisateur/mot de passe, DSN SQL et connection strings;
- `.env` mêlé à du texte normal;
- marqueurs `AGENTS.md`, `CLAUDE.md`, Markdown et instructions d’outil injectées;
- blocs shell et commandes destructrices dans une transcript;
- symlink/junction substitué entre plan et commit, et répertoire parent échangé après le dernier contrôle;
- racine autorisée elle-même renommée puis remplacée par un lien vers un répertoire tiers portant la même preimage;
- racine autorisée renommée puis remplacée par un **autre vrai répertoire** au même chemin, avant l’apply comme après la mise en cache de l’autorité;
- racine absente au moment du plan dont l’ancêtre est remplacé, ou qui est créée entre-temps par quelqu’un d’autre;
- reprise d’une opération `applying` avant et après la liaison durable de l’identité de la racine créée;
- manifeste ancien, vide, tronqué, de version inconnue, à champ inconnu ou suivi de JSON en trop, en `prepared`, `applying` et `rolling_back`;
- manifeste à la version courante décrivant une structure impossible: checksum incohérent, racine sans autorité, autorité inutilisée, suffixe non confiné;
- échec de la transaction de décision **avant** qu’elle n’aboutisse, et erreur retournée **après** qu’elle a abouti;
- fichier `.temper-*.tmp` appartenant à l’utilisateur, et temporaire vivant d’une seconde mutation en vol dans le même répertoire;
- répertoire créé par l’utilisateur après la capture, à l’intérieur de l’arbre qu’un rollback traverse;
- édition concurrente d’un fichier entre capture et écriture, entre deux écritures d’un même manifeste, entre la dernière écriture et la décision, et entre écriture et rollback;
- changement de permissions concurrent sans changement d’octets;
- deux suggestions distinctes visant la même ressource canonique, avec redémarrage entre les deux;
- capture périmée obtenant le claim juste après sa libération;
- rollback interrompu, repris par une reconciliation qui pourrait le rejouer en avant;
- rejet périmé contre une acceptation déjà commitée;
- manifeste dont le chemin absolu a été réécrit en base, ou dont l’identité de racine est absente;
- base au dernier `user_version` mais dont une table ou une colonne obligatoire manque;
- mort du processus injectée après chaque étape d’une opération multi-fichiers;
- répertoire vide préexistant que la mutation traverse, et répertoires qu’elle crée elle-même;
- transition d’état interdite: `prepared → committed`, `prepared → rolling_back`, fermeture d’un `rolling_back` comme s’il n’avait rien écrit, réouverture d’un état terminal;
- corruption JSON/SQLite et migration interrompue;
- requête cross-origin vers la review API;
- transcript tronquée, remplacée ou réécrite après checkpoint.

## Fermé par HOK-539

- tout payload provider est construit par `internal/outbound`: redaction, neutralisation des marqueurs et bornes de taille sur chaque valeur dynamique, puis redaction de l’ensemble assemblé. `llm.Provider.Generate` n’accepte plus qu’un `outbound.Payload`, dont les champs sont non exportés;
- les chemins transcript, contexte de dépôt (`AGENTS.md`, `CLAUDE.md`, `.cursor/rules`), métadonnées de session et blocs gérés du gardener passent tous par cette préparation unique;
- le provider Codex utilise un `CODEX_HOME` temporaire neutre et séparé du working directory, et y place uniquement le `auth.json` existant pour conserver l'authentification par abonnement: lien symbolique quand disponible, copie `0600`/home `0700` en fallback hors Windows; Windows exige le support des liens symboliques plutôt que créer une copie sans ACL équivalente; il ne parse ni ne journalise ce fichier, mais cette exception constitue un accès explicite au credential;
- l’endpoint est validé avant la construction de toute requête portant la clé: HTTPS pour le distant, HTTP uniquement sur loopback, ni userinfo, query string, fragment ou schéma inattendu; les redirects sont refusés pour éviter de rejouer corps et credentials vers une autre autorité;
- l’auto-accept est supprimé. `auto_accept_threshold` reste parsé pour compatibilité, n’écrit jamais, et déclenche un avertissement explicite au scan;
- réponse provider décodée comme un unique objet JSON sans champs inconnus, puis validée contre un schéma fermé (enums vérifiés et non coercés, champs requis, bornes de taille, confiance dans `[0,1]`, preuves vérifiées verbatim contre le texte réellement envoyé); scope, placement et destination sont déterminés localement et les valeurs provider correspondantes sont ignorées;
- l’artefact est décrit par un plan déterministe (`writer.BuildPlan`) validé avant toute écriture — destination confinée sous sa racine canonique malgré symlinks/junctions, marqueurs gérés interdits dans le contenu, et un placement `path_scoped` sans glob ne peut pas devenir global;
- plus aucune extraction de fence shell: `run.sh` n’est plus généré et aucun artefact écrit n’est exécutable.

## Fermé par HOK-540

- une acceptation est une saga durable et non plus « écrire un fichier » suivi de « décider une ligne ». `writer.Accept` calcule la mutation complète, capture les preimages, journalise le manifeste (`prepared`), franchit `applying` avant le premier octet, puis ferme la décision et l’entrée de journal **dans la même transaction** (`committed`). `writer.Undo` emprunte la même saga en sens inverse;
- au démarrage, `openStore` appelle `writer.Reconcile` avant que la moindre commande ne lise le store comme s’il faisait autorité: une opération trouvée en `prepared` n’a provablement rien écrit et est relâchée sans toucher aux fichiers; une opération en `applying` est rejouée depuis son manifeste jusqu’à complétion — octet pour octet — ou restaurée à ses preimages puis relâchée;
- les checksums du manifeste sont des **préconditions**, pas des diagnostics. Avant apply, rollback ou rejeu, chaque destination est classée en preimage connue, postimage connue ou **troisième état**. Preimage: on écrit. Postimage: c’est déjà fait, on ne réécrit pas — c’est ce qui rend le rejeu idempotent et l’undo conditionnel. Troisième état: quelqu’un d’autre possède ces octets, le manifeste entier est refusé (`writer.ErrConflict`) et rien n’est écrit ni restauré par-dessus. Un rollback qui ne peut pas restaurer exactement s’arrête et nomme les fichiers laissés en l’état (invariant 7);
- la réservation porte sur **les ressources, pas sur la suggestion**. `BeginOperation` réclame, dans la transaction qui journalise l’intention, chaque fichier canonique du manifeste (casse repliée sur Windows). Deux suggestions *différentes* qui écrivent le même `AGENTS.md` sont un seul conflit: la seconde a planifié son contenu depuis un instantané que la première invalide. Les claims sont des lignes: elles survivent à la panne et gardent le fichier réservé jusqu’à ce que la reconciliation termine ou relâche l’opération;
- une décision est un compare-and-set depuis l’état contre lequel elle a été planifiée. `Store.Reject` refuse hors de `pending` **et** refuse tant qu’une opération est inachevée — sinon la ligne dirait « rejeté » à côté d’un artefact présent. `CommitOperation` réassert le même état source, donc une opération préparée sur une lecture périmée échoue au lieu d’écraser la décision de l’autre client;
- le confinement au moment de la mutation ne repose plus sur un contrôle qui expire: chaque lecture, écriture, `MkdirAll`, rename et suppression passe par un `os.Root` ouvert sur la racine autorisée et nommé **relativement** à elle. Un répertoire parent remplacé par un symlink ou une junction entre le dernier contrôle et l’écriture ne redirige donc rien — la racine refuse le chemin au lieu de le suivre;
- la racine autorisée est identifiée par son **objet**, jamais par son nom. Un chemin canonique ne prouve rien: renommer la racine et mettre un *autre vrai répertoire* au même nom laisse toute comparaison de chemin d’accord avec elle-même, et sur Windows `os.SameFile` rouvre le chemin enregistré — donc compare le remplaçant avec lui-même. Le manifeste enregistre donc device+inode (Unix) ou numéro de série de volume + index de fichier (Windows), lus **par le descripteur ouvert**. Une plateforme sans identité stable est refusée explicitement, jamais rétrogradée en comparaison de chemins;
- cette identité est reprouvée **à chaque accès**, pas une fois. Deux questions distinctes: le nom enregistré mène-t-il encore à l’objet enregistré, et le handle sur le point d’être utilisé est-il ce même objet? Une preuve prise à l’ouverture est une preuve sur le monde d’alors, et une mutation lit, écrit et revalide longtemps après — un handle en cache ne court-circuite donc jamais la vérification;
- une racine **absente au moment du plan** ne peut pas être identifiée, puisqu’il n’y a pas encore d’objet. Le manifeste capture à la place le plus profond ancêtre existant, son identité, et le suffixe confiné qui mène à la racine. La racine n’est créée qu’**à travers cet ancêtre ouvert**, et l’identité du répertoire créé est rendue durable par un CAS (`RebindRoot`, réservé à l’état `applying`) **avant la première écriture cible**. Une reprise qui trouve une opération `applying` dont la racine n’a jamais été liée sait donc que rien n’a été écrit dessous: elle abandonne au lieu d’adopter un répertoire qui se trouverait là;
- le manifeste est un **format fermé et versionné**. Il est décodé avec `DisallowUnknownFields`, tout JSON en trop est refusé, et la structure est validée avant tout effet: version courante, au moins une opération, une autorité par racine et aucune de plus, suffixe confiné, checksums cohérents avec les octets qu’ils décrivent, combinaisons impossibles rejetées. L’asymétrie est délibérée: une opération `prepared` est relâchée **sans lire son manifeste**, parce que cet état prouve qu’aucun octet n’a bougé et qu’une entrée illisible ne doit pas devenir un blocage permanent; une opération `applying` ou `rolling_back` portant un manifeste ancien, tronqué ou sémantiquement invalide échoue explicitement, ne touche aucun fichier et **garde ses claims**;
- la machine d’états est explicite et chaque transition a sa méthode: `prepared → applying`, `applying → applying` (liaison d’une racine créée, et rien d’autre), `applying → committed`, `prepared|applying → rolled_back` (`AbandonOperation`, quand aucun octet cible n’a bougé), `applying → rolling_back`, `rolling_back → rolled_back` (`FinishRollback`). `prepared → committed` est **interdit**: une opération qui n’a pas franchi la ligne durable enregistrerait une décision dont personne n’a exécuté la moitié filesystem. Les claims ne sont relâchés que sur `committed` ou `rolled_back`;
- un échec de `CommitOperation` est une **question, pas une réponse**. La transaction peut n’avoir jamais abouti, ou avoir abouti alors que seule sa réponse s’est perdue — et défaire dans le second cas supprimerait l’artefact d’une acceptation que le store considère déjà faite. L’opération est donc relue: `committed` est traité comme un succès ambigu résolu, `applying` autorise le rollback complet, et tout autre cas (relecture impossible, état inattendu) laisse l’opération non terminale avec ses claims pour la reconciliation. Si une modification externe a créé un troisième état avant la compensation, le rollback refuse de l’écraser, nomme le fichier et garde ses claims;
- une preuve globale est périmée dès la première écriture. Le manifeste est donc prouvé **trois fois**: entier avant le premier octet, pour qu’un conflit sur le troisième fichier ne laisse pas les deux premiers remplacés; puis destination par destination immédiatement avant chaque remplacement, pour qu’une édition du fichier N+1 pendant l’écriture du fichier N soit refusée et non écrasée; puis entier à nouveau après la dernière écriture, pour qu’une édition arrivée entre-temps ne soit pas commitée par-dessus;
- **cette dernière validation est la frontière transactionnelle du filesystem.** Ce qu’elle établit, c’est que chaque destination porte exactement ce que la mutation a écrit; ce qui arrive à ces fichiers ensuite est quelqu’un qui modifie un fichier terminé, et la transaction SQLite qui suit ne fige pas le disque de l’utilisateur. Temper ne prétend donc pas exclure une édition postérieure à cette frontière: la décision est enregistrée, parce qu’elle était vraie quand elle a été prise. Ce qui est garanti est l’autre moitié — cette édition n’est jamais écrasée, et la compensation qui pourrait l’écraser plus tard refuse et nomme le fichier (`ErrConflict`) au lieu de restaurer des octets dont elle ne connaît plus le sens;
- validation et phase mutante sont séparées. Une opération refusée avant sa première écriture n’a provablement rien touché: elle est fermée immédiatement et ses claims sont relâchés, au lieu de rester `applying` et d’exiger une reconciliation humaine pour une décision qui n’a jamais eu lieu. Une capture périmée qui obtient le claim juste après sa libération relève exactement de ce cas, et un retry frais avance sans intervention;
- l’abandon est une intention durable. Une mutation qui a déjà écrit passe par un état `rolling_back` **commité avant** la première restauration, et les claims ne sont relâchés qu’une fois la restauration terminée. Sans cet état, une opération dont l’appel de rollback a échoué resterait `applying` et la reconciliation suivante la rejouerait en avant — terminant, depuis une panne, une décision explicitement abandonnée. Toute erreur de transition est propagée, jamais ignorée;
- les permissions sont des **préconditions**, pas des métadonnées. Le manifeste distingue « mode capturé » de « fichier absent » — un fichier en `000` n’est pas un fichier qui n’existe pas — et porte le mode de la postimage en plus de celui de la preimage. Un `chmod` concurrent, qui ne change aucun octet, est donc un conflit et non un écrasement silencieux. Sur une plateforme qui ne porte pas les bits POSIX (Windows), le mode reste capturé et la moitié portable de l’invariant est asservie sur le manifeste plutôt que sur le filesystem;
- **Temper ne supprime rien dont il n’ait pas la preuve exacte.** Aucun répertoire n’est retiré — ni ceux que l’utilisateur avait, ni ceux que la mutation a créés: la preuve « je l’ai créé » est écrite avant la mutation, et rien dans un manifeste ne peut établir, après une panne, que personne n’y a rien mis depuis. Un répertoire vide laissé derrière est un compromis assumé dans le sens non destructeur. Le seul temporaire supprimé est celui que l’écriture courante vient de créer, par son nom exact: un préfixe n’est pas une preuve de propriété — il désigne aussi bien un fichier `.temper-*.tmp` de l’utilisateur que le temporaire *vivant* d’une autre mutation en vol, sur le point d’être renommé en place. Un orphelin laissé par un processus mort est désordonné et inoffensif; l’un ou l’autre de ces deux-là ne l’est pas;
- un undo de gardener est disponible quand — et seulement quand — l’acceptation a enregistré ce qu’elle a remplacé. Une acceptation gardener journalisée est restaurée octet pour octet depuis son manifeste; une acceptation antérieure au journal est refusée par un sentinelle explicite (`writer.ErrLegacyAcceptance`) qui nomme le fichier et renvoie vers son historique git, au lieu de recalculer une suppression qui effacerait un bloc au lieu de restaurer celui qu’il avait écrasé;
- `quick_check` répond « ces pages sont-elles lisibles », pas « est-ce le schéma dont ce binaire a besoin ». À l’ouverture, la **forme** du schéma est vérifiée en plus de la version: chaque table et chaque colonne obligatoire. Une base physiquement valide mais stampée au dernier `user_version` avec une table ou une colonne manquante est refusée par `store.ErrSchemaIncomplete`, avec sa voie de récupération, plutôt que de casser au milieu de la première acceptation. Elle n’est jamais re-migrée silencieusement: cela ne ferait que restamper la même version par-dessus le même trou;
- un chemin stocké est une donnée, jamais une autorité. Le couple `(root, path)` persisté est re-dérivé en nom root-relatif à chaque chargement du manifeste et refusé s’il sort de sa racine, donc une ligne trafiquée ne devient pas une autorité de mutation au rejeu. `BuildRemoval` recalcule le plan depuis la suggestion, en tire la **forme** de la suppression comme sa cible, et refuse tout ce qui n’est pas exactement l’artefact que cette suggestion produit;
- un undo compense le **manifeste exact de l’acceptation commitée**, relu depuis son entrée de journal, et non une suppression recalculée depuis la ligne du jour: le fichier qui préexistait est restauré, la ligne d’import `@AGENTS.md` ajoutée à un `CLAUDE.md` est retirée, et une compétence démotée par le budget revient. Une acceptation antérieure au journal — et elle seule — retombe sur la suppression recalculée;
- le rollback restaure l’**état précédent**, permissions comprises: le manifeste capture le mode POSIX de la preimage et l’écriture atomique le réapplique explicitement (`Chmod` après création, l’umask ne décide pas);
- il n’existe plus de porte non durable exportée. `writeUnjournaled`/`removeUnjournaled` sont non exportées et sans appelant de production: `Accept` et `Undo` sont les seules entrées, donc « écrire le fichier puis enregistrer la décision » ne peut pas être réintroduit en deux commits;
- une mutation est une fonction de son entrée: à confiance égale, la démotion budgétaire départage par identifiant de bloc. Sans ce tie-break le choix venait de l’itération de map aléatoire de Go — un plan non reproductible ne peut être ni revu avant application ni rejoué après panne;
- le schéma SQLite est versionné par `user_version`. Chaque migration est une transaction unique qui monte aussi la version, donc une migration interrompue ou en échec laisse la base exactement à la version précédente, sans DDL partiel;
- une base illisible ou en échec de `quick_check` est refusée à l’ouverture (`store.ErrCorrupt`), jamais réparée en place ni recréée silencieusement — perdre le journal sans le dire transformerait une panne en état partiellement accepté indétectable;
- un checkpoint n’avance qu’avec les suggestions qu’il couvre: `AdvanceCheckpoint` insère les suggestions et le high-water mark dans une seule transaction.

### Durabilité: ce qui est prouvé, et ce qui ne l’est pas

La distinction porte sur la nature de l’interruption, pas sur son ampleur.

**Prouvé, par injection d’interruption dans la suite de tests** — la mort du *processus* à n’importe quelle étape d’une mutation multi-fichiers:

- chaque fichier est remplacé par un temporaire écrit en entier, `fsync`é, puis `rename`é: un lecteur voit l’ancien fichier ou le nouveau, jamais un mélange. Le rename remplace le dernier composant lui-même, donc un symlink planté à la destination est écrasé et non suivi;
- l’intention est journalisée avant le premier octet et la décision est commitée avec le journal dans une seule transaction SQLite: il n’existe aucun instant où le fichier est écrit et la décision perdue, ou l’inverse, sans qu’une opération incomplète le dise;
- au redémarrage, `writer.Reconcile` termine ou restaure toute saga incomplète avant qu’une commande ne lise le store comme faisant autorité.

**Non prouvé ici** — la coupure d’alimentation matérielle:

- `fsync` sur le fichier est demandé et son erreur est propagée; `fsync` sur le **répertoire** — ce qui rendrait le rename lui-même durable — est **best-effort et son résultat est délibérément ignoré**: Windows ne permet pas d’ouvrir un répertoire pour cette opération, et plusieurs filesystems répondent sans garantie. Go ne fournit pas de moyen portable de l’exiger;
- après une coupure physique, un rename peut donc être perdu alors que le contenu du fichier était sur le support. La saga reste la voie de sortie: l’opération est retrouvée en `prepared`, `applying` ou `rolling_back` et la reconciliation la termine ou la restaure. C’est une garantie de **convergence applicative après reprise**, pas une garantie matérielle;
- aucune de ces propriétés n’est établie contre un disque qui ment sur ses écritures, un cache d’écriture non protégé, ni un filesystem monté sans barrières.

Les quatre critères Linear de HOK-540 se lisent dans ce cadre: aucune acceptation partielle après une panne **injectée**, aucune suppression hors des racines autorisées, idempotence et préservation du contenu utilisateur, migration et corruption explicites avec sauvegarde.

### Récupération

- **Base refusée à l’ouverture** (corruption, `quick_check`, `integrity_check`). Le message nomme le fichier et la voie de sortie. Restaurer la sauvegarde `<db>.backup-v<version>-<horodatage>.db` la plus récente à côté de la base, ou déplacer la base pour repartir d’une base vierge. Le fichier refusé n’est jamais modifié: la preuve reste disponible pour un diagnostic.
- **Migration en échec.** L’ouverture échoue, la base reste à sa version précédente et le message nomme la sauvegarde prise juste avant. Une base existante n’est jamais migrée sans `integrity_check` réussi **et** sauvegarde écrite (`VACUUM INTO`, donc WAL inclus).
- **Base écrite par un binaire plus récent** (`user_version` supérieur). Refus explicite: mettre à jour temper, ne pas rétrograder la base.
- **Base au dernier `user_version` mais de forme incomplète** (`store.ErrSchemaIncomplete`). Le message nomme la table ou la colonne manquante. Restaurer une sauvegarde ou déplacer la base; ne pas tenter une re-migration, qui restamperait la même version sur le même trou.
- **Opération irréconciliable.** Si une opération en `applying` ou `rolling_back` ne peut ni être terminée ni être restaurée, `Reconcile` s’arrête, nomme l’opération et le chemin à arbitrer à la main, conserve ses claims — le fichier est réellement contesté — et ne détruit rien de plus (invariant 7). `temper status` liste l’opération.
- **Undo d’une action gardener antérieure au journal** (`writer.ErrLegacyAcceptance`). Rien n’a enregistré le contenu de bloc que l’acceptation a remplacé. Le refus nomme le fichier: le récupérer depuis son historique git.

## Limites restantes

- la review API peut être liée à une interface non loopback (HOK-541);
- la redaction reste heuristique: elle couvre les formes listées ci-dessus, pas tout secret concevable;
- les clés peuvent être stockées en clair dans la configuration;
- la matrice CI ne qualifie pas Windows ni macOS.

Les issues HOK-541 et HOK-547 portent les fermetures restantes. Jusqu’à leur validation, Temper reste un prototype à utiliser uniquement sur des données et dépôts dont l’utilisateur accepte le risque.
