# ADR 0006 — Identidade verificada e isolamento por provedor

## Decisão

Usar Keycloak com `client_credentials` e go-oidc para validar assinatura RS256,
issuer, audience e expiração. Verificar também `nbf`, subject e roles.
A role `provider` habilita a identidade `providerId`; `internal` permite operações
de carteira. Autorizar no caso de uso antes do repositório e filtrar consultas
externas pelo provedor autenticado. `identity.Principal` não carrega JWT ou HTTP.

HTTP e SQS seguem a mesma autorização. Hoje, SQS exige `AccessToken` na mensagem.
Tokens podem expirar antes da entrega; identidade durável de origem e políticas
segregadas do broker ainda precisam substituir essa solução inicial.

## Motivo

O token verificado estabelece a identidade; o `providerId` enviado no corpo
não comprova quem está chamando. Autorizar no caso de uso mantém a mesma proteção
em HTTP e SQS, e filtrar pelo provedor impede acesso a transações de terceiros.
